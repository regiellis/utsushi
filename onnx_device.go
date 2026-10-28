package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	ort "github.com/yalue/onnxruntime_go"
)

// toolDevice is where the model tools run: this machine's NVIDIA GPU
// through ONNX Runtime's CUDA provider when there is one, else the CPU.
// UTSUSHI_TOOLS_DEVICE=cpu keeps them on the CPU.
type toolDevice struct {
	GPU  bool
	Name string // "NVIDIA RTX 4000 SFF Ada Generation", or "CPU"
}

// findDevice looks for a CUDA device: the driver's control node and a GPU
// nvidia-smi can name.
func findDevice() toolDevice {
	cpu := toolDevice{Name: "CPU"}
	if strings.EqualFold(os.Getenv("UTSUSHI_TOOLS_DEVICE"), "cpu") || modelPlatform() != nil {
		return cpu
	}
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		return cpu
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader", "--id=0").Output()
	if err != nil {
		return cpu
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return cpu
	}
	return toolDevice{GPU: true, Name: name}
}

// runtimeFiles are the ONNX Runtime files a device needs.
func runtimeFiles(d toolDevice) []modelFile {
	if d.GPU {
		return []modelFile{ortGPU, cudaRuntime, cublas, curand, cudnn}
	}
	return []modelFile{ortRuntime}
}

// cudaPreload are the libraries the CUDA provider links by name, and cuDNN,
// which it opens by name. None is on the system's library path, and
// LD_LIBRARY_PATH is read only when a process starts, so Utsushi opens
// each by its full path first, globally, and the provider's lookups find
// them loaded. cuBLAS and cuDNN find their own siblings through $ORIGIN.
var cudaPreload = []string{"libcudart.so.13", "libcublasLt.so.13", "libcublas.so.13", "libcurand.so.10", "libcudnn.so.9"}

func preloadLibrary(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if C.dlopen(p, C.RTLD_NOW|C.RTLD_GLOBAL) == nil {
		return fmt.Errorf("load %s: %s", filepath.Base(path), C.GoString(C.dlerror()))
	}
	return nil
}

// ortEnv is ONNX Runtime's one environment per process, and the device
// its sessions run on.
var ortEnv struct {
	sync.Mutex
	ready  bool
	gpu    bool
	warned bool // a CUDA provider that would not attach was logged
	// cpu keeps new sessions off the GPU for the job being retried there
	// (runTool). Model jobs run one at a time, so it covers only that one.
	cpu bool
}

// sessionsOnCPU keeps new sessions on the CPU until it is called with false.
func sessionsOnCPU(on bool) {
	ortEnv.Lock()
	ortEnv.cpu = on
	ortEnv.Unlock()
}

// gpuFull reports whether err is the GPU running out of memory: the CUDA
// provider's arena or CUDA itself refusing an allocation.
func gpuFull(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"Failed to allocate memory", "out of memory", "CUDNN_STATUS_ALLOC_FAILED", "cudaErrorMemoryAllocation"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// gpuHolders names the other programs holding the GPU's memory, largest
// first, as nvidia-smi reports them ("python, 15.2 GB"), or "" when it
// cannot say.
func gpuHolders() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-compute-apps=pid,process_name,used_memory",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return ""
	}
	var held []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ", ")
		if len(f) != 3 || f[0] == strconv.Itoa(os.Getpid()) {
			continue
		}
		mib, err := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		if err != nil {
			continue
		}
		held = append(held, fmt.Sprintf("%s, %.1f GB", filepath.Base(strings.TrimSpace(f[1])), mib/1024))
	}
	return strings.Join(held, "; ")
}

// startORT loads the device's runtime once per process. The first device
// wins: a process cannot load a second ONNX Runtime library.
func startORT(m *models, d toolDevice) error {
	ortEnv.Lock()
	defer ortEnv.Unlock()
	if ortEnv.ready {
		return nil
	}
	lib := m.path(ortRuntime)
	if d.GPU {
		for _, name := range cudaPreload {
			if err := preloadLibrary(filepath.Join(m.dir, "cuda", name)); err != nil {
				return err
			}
		}
		lib = m.path(ortGPU)
	}
	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("start ONNX Runtime: %w", err)
	}
	ortEnv.ready, ortEnv.gpu = true, d.GPU
	return nil
}

// sessionOptions puts a new session on the GPU when the runtime is the
// GPU build. A CUDA provider that will not attach (a driver too old, a
// busy device) leaves the session on the CPU, said once in the log.
// The caller destroys the options once the session is made.
func sessionOptions() (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	ortEnv.Lock()
	gpu := ortEnv.gpu && !ortEnv.cpu
	ortEnv.Unlock()
	if !gpu {
		return opts, nil
	}
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil {
		return nil, errors.Join(err, opts.Destroy())
	}
	defer func() {
		if err := cuda.Destroy(); err != nil {
			log.Printf("utsushi: free the CUDA options: %v", err)
		}
	}()
	err = cuda.Update(map[string]string{"device_id": "0"})
	if err == nil {
		err = opts.AppendExecutionProviderCUDA(cuda)
	}
	if err != nil {
		ortEnv.Lock()
		if !ortEnv.warned {
			ortEnv.warned = true
			log.Printf("utsushi: the model tools run on the CPU: the CUDA provider would not attach: %v", err)
		}
		ortEnv.Unlock()
	}
	return opts, nil
}

// withOptions makes a session with sessionOptions and frees the options.
func withOptions[T any](make func(*ort.SessionOptions) (T, error)) (T, error) {
	opts, err := sessionOptions()
	if err != nil {
		var zero T
		return zero, err
	}
	s, err := make(opts)
	return s, errors.Join(err, opts.Destroy())
}
