package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The model tools run in this process through ONNX Runtime, not on
// ComfyUI. The runtime libraries and the models download on first use into
// data/models, each pinned by URL and SHA-256 (read 2026-10-18), so a
// changed upstream file is refused rather than loaded.
type modelFile struct {
	// Name is the file kept, under Dir: the download itself, or for an
	// archive the member the code loads.
	Name   string
	Dir    string // a folder under data/models, or "" for data/models
	URL    string
	SHA256 string // of the download: the archive itself for an archive
	Size   int64
	// Members are, for an archive (.tgz or a .whl, which is a zip), the
	// files kept from it by their path inside, each under Dir by its base
	// name.
	Members []string
}

// ortRuntime is ONNX Runtime 1.31.0, CPU build. onnxruntime_go v1.36.0
// asks for C API 29 (1.29), which newer runtimes still serve.
var ortRuntime = modelFile{
	Name:    "libonnxruntime.so.1.31.0",
	URL:     "https://github.com/microsoft/onnxruntime/releases/download/v1.31.0/onnxruntime-linux-x64-1.31.0.tgz",
	SHA256:  "cc5c72baf5ae5c8238a6841f0897227be2d02826b9cf98eaf02fdefaeeb03a57",
	Size:    11408003,
	Members: []string{"onnxruntime-linux-x64-1.31.0/lib/libonnxruntime.so.1.31.0"},
}

// The GPU runtime: ONNX Runtime 1.31.0's CUDA 13 build (ORT's default
// since 1.27; driver 580 is the CUDA 13.0 branch) and the libraries its
// CUDA provider needs, from NVIDIA's own PyPI wheels: the CUDA runtime,
// cuBLAS and cuBLASLt, cuRAND, and cuDNN 9 for CUDA 13. All land flat in
// data/models/cuda; each NVIDIA library finds its siblings through its
// $ORIGIN runpath. About 1.2 GB in all, downloaded once.
var (
	ortGPU = modelFile{
		Name: "libonnxruntime.so.1.31.0", Dir: "cuda",
		URL:    "https://github.com/microsoft/onnxruntime/releases/download/v1.31.0/onnxruntime-linux-x64-gpu_cuda13-1.31.0.tgz",
		SHA256: "2867dec99c6a4926e8142361b9d4d1518c16c85b854b4d41d28d65d3b2c81135",
		Size:   244574938,
		Members: []string{
			"onnxruntime-linux-x64-gpu_cuda13-1.31.0/lib/libonnxruntime.so.1.31.0",
			"onnxruntime-linux-x64-gpu_cuda13-1.31.0/lib/libonnxruntime_providers_shared.so",
			"onnxruntime-linux-x64-gpu_cuda13-1.31.0/lib/libonnxruntime_providers_cuda.so",
		},
	}
	cudaRuntime = modelFile{
		Name: "libcudart.so.13", Dir: "cuda",
		URL:     "https://files.pythonhosted.org/packages/98/8a/3431271f6344874b8f1ac03f16b3d679c91493f8da63f716160403e6d0a0/nvidia_cuda_runtime-13.4.92-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl",
		SHA256:  "9641f797da20ce1dd8e779b6e96d08cf9ba564cec8e8225458811ee26423f3a5",
		Size:    2494438,
		Members: []string{"nvidia/cu13/lib/libcudart.so.13"},
	}
	cublas = modelFile{
		Name: "libcublas.so.13", Dir: "cuda",
		URL:     "https://files.pythonhosted.org/packages/eb/1b/adc70acf3fde509b1f0ca29d1c0e8d9b00aaecd871f8cf877257c8b68d9c/nvidia_cublas-13.8.1.7-py3-none-manylinux_2_27_x86_64.whl",
		SHA256:  "c11a27fd4379510e5b1f84b367a2514d1e52fe5cc13442117a0e0a1addee3cf2",
		Size:    439315821,
		Members: []string{"nvidia/cu13/lib/libcublas.so.13", "nvidia/cu13/lib/libcublasLt.so.13"},
	}
	curand = modelFile{
		Name: "libcurand.so.10", Dir: "cuda",
		URL:     "https://files.pythonhosted.org/packages/07/73/3ee8e5b4cb891401e603ffd3a59b35c6afe785fd2de123afe7c7029603dc/nvidia_curand-10.4.4.72-py3-none-manylinux_2_27_x86_64.whl",
		SHA256:  "25c3457ae7a224fdd484dab90b0fc5dc0e842fab5db3012afa4a5bd2af4eb7e5",
		Size:    61498332,
		Members: []string{"nvidia/cu13/lib/libcurand.so.10"},
	}
	cudnn = modelFile{
		Name: "libcudnn.so.9", Dir: "cuda",
		URL:    "https://files.pythonhosted.org/packages/af/75/96ea5c5368eb595c39d629cde08a66227a864e71ccb0e593add2612bb952/nvidia_cudnn_cu13-9.27.0.42-py3-none-manylinux_2_27_x86_64.whl",
		SHA256: "9677e76f21862eb5da7ee5ed69d544738b2d8b5c3ce7e5ec125c5592e6cdbdc8",
		Size:   536771498,
		Members: []string{"nvidia/cudnn/lib/libcudnn.so.9", "nvidia/cudnn/lib/libcudnn_adv.so.9", "nvidia/cudnn/lib/libcudnn_cnn.so.9",
			"nvidia/cudnn/lib/libcudnn_engines_precompiled.so.9", "nvidia/cudnn/lib/libcudnn_engines_runtime_compiled.so.9",
			"nvidia/cudnn/lib/libcudnn_engines_tensor_ir.so.9", "nvidia/cudnn/lib/libcudnn_ext.so.9", "nvidia/cudnn/lib/libcudnn_graph.so.9",
			"nvidia/cudnn/lib/libcudnn_heuristic.so.9", "nvidia/cudnn/lib/libcudnn_ops.so.9"},
	}
)

// Video Depth Anything Small as TheoreticallyPose's ONNX export splits it,
// a per-frame encoder and a 32-frame temporal head fixed at 518x910, and
// Depth Anything V2 Small. Both are Apache-2.0; their Base and Large
// models are CC-BY-NC and are not used.
var (
	vdaEncoder = modelFile{
		Name:   "vda_vits_encoder_518x910_fp16.onnx",
		URL:    "https://huggingface.co/TheoreticallyTim/theoreticallypose-vda/resolve/9360ac2b147da3ca220fb9e1bb55600fd7e3d9aa/vda_vits_encoder_518x910_fp16.onnx",
		SHA256: "9e05204252f8cdd39e3151541ebded90aa07fe1026cb03d0996748f518e56512",
		Size:   45264674,
	}
	vdaHead = modelFile{
		Name:   "vda_vits_head_T32_37x65_fp16.onnx",
		URL:    "https://huggingface.co/TheoreticallyTim/theoreticallypose-vda/resolve/9360ac2b147da3ca220fb9e1bb55600fd7e3d9aa/vda_vits_head_T32_37x65_fp16.onnx",
		SHA256: "8037ae4cb8f6b775599fae31badb4abd06ab4e5f9c26b86e70d21c7f1e3a4f7a",
		Size:   14160330,
	}
	dav2Small = modelFile{
		Name:   "depth_anything_v2_small.onnx",
		URL:    "https://huggingface.co/onnx-community/depth-anything-v2-small/resolve/4472b7362082ad9968fee890ca0f1e5aca36b93d/onnx/model.onnx",
		SHA256: "afb6a5c28f3b6bf1618c6e43f02073ef9dfdc70e937502d51603e57b0a1df10c",
		Size:   99060839,
	}
)

// modelPlatform says why the model tools cannot run here, or nothing.
func modelPlatform() error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errOffline{fmt.Sprintf("The model tools run on Linux x64; this server is %s/%s.", runtime.GOOS, runtime.GOARCH)}
	}
	return nil
}

// models keeps the depth files in one folder.
type models struct {
	dir    string
	client *http.Client
}

func newModels(dir string) *models {
	return &models{dir: dir, client: &http.Client{Timeout: 20 * time.Minute}}
}

func (m *models) path(f modelFile) string { return filepath.Join(m.dir, f.Dir, f.Name) }

// kept is where each file f keeps lands.
func (m *models) kept(f modelFile) []string {
	if len(f.Members) == 0 {
		return []string{m.path(f)}
	}
	out := make([]string, len(f.Members))
	for i, member := range f.Members {
		out[i] = filepath.Join(m.dir, f.Dir, filepath.Base(member))
	}
	return out
}

func (m *models) have(f modelFile) bool {
	for _, p := range m.kept(f) {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// missing is how many bytes the files not yet downloaded weigh.
func (m *models) missing(files ...modelFile) int64 {
	var n int64
	for _, f := range files {
		if !m.have(f) {
			n += f.Size
		}
	}
	return n
}

// ensure downloads whichever files are missing. progress gets the bytes
// done and the total across them.
func (m *models) ensure(ctx context.Context, progress func(done, total int64), files ...modelFile) error {
	total := m.missing(files...)
	var done int64
	for _, f := range files {
		if m.have(f) {
			continue
		}
		base := done
		if err := m.fetch(ctx, f, func(n int64) { progress(base+n, total) }); err != nil {
			return fmt.Errorf("download %s: %w", f.Name, err)
		}
		done += f.Size
	}
	return nil
}

// fetch downloads one file to a temporary name in its folder, checks its
// hash, and only then renames it, or for an archive extracts its members,
// into place.
func (m *models) fetch(ctx context.Context, f modelFile, progress func(int64)) error {
	dir := filepath.Join(m.dir, f.Dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("utsushi: close the %s download: %v", f.Name, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("utsushi: remove %s: %v", tmp.Name(), err)
		}
	}()
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), &countingReader{r: resp.Body, each: progress})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("its SHA-256 is %s, not the pinned %s", got, f.SHA256)
	}
	if len(f.Members) == 0 {
		return os.Rename(tmp.Name(), m.path(f))
	}
	kept := m.kept(f)
	want := make(map[string]string, len(f.Members))
	for i, member := range f.Members {
		want[member] = kept[i]
	}
	if strings.HasSuffix(f.URL, ".whl") {
		err = extractZip(tmp.Name(), want)
	} else {
		err = extractTgz(tmp.Name(), want)
	}
	return err
}

// keepMember writes one archive member to dst through a .part file.
func keepMember(r io.Reader, size int64, dst string) error {
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o750)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.Join(err, os.Remove(part))
	}
	return os.Rename(part, dst)
}

// missingMembers names the members an archive did not hold.
func missingMembers(want map[string]string, found map[string]bool) error {
	var gone []string
	for member := range want {
		if !found[member] {
			gone = append(gone, member)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("the archive has no %s", strings.Join(gone, ", "))
	}
	return nil
}

// extractZip keeps the wanted members of a zip (a Python wheel).
func extractZip(archive string, want map[string]string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() {
		if err := zr.Close(); err != nil {
			log.Printf("utsushi: close %s: %v", archive, err)
		}
	}()
	found := map[string]bool{}
	for _, zf := range zr.File {
		dst, ok := want[zf.Name]
		if !ok {
			continue
		}
		r, err := zf.Open()
		if err != nil {
			return err
		}
		err = keepMember(r, int64(zf.UncompressedSize64), dst)
		if cerr := r.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		found[zf.Name] = true
	}
	return missingMembers(want, found)
}

// extractTgz keeps the wanted members of a .tgz.
func extractTgz(archive string, want map[string]string) error {
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			log.Printf("utsushi: close %s: %v", archive, err)
		}
	}()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return missingMembers(want, found)
		}
		if err != nil {
			return err
		}
		dst, ok := want[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := keepMember(tr, hdr.Size, dst); err != nil {
			return err
		}
		found[hdr.Name] = true
	}
}

// countingReader reports the bytes read so far.
type countingReader struct {
	r    io.Reader
	n    int64
	each func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.each(c.n)
	return n, err
}

// The other model tools' files, checked 2026-10-18. Pose is DWPose's
// pair: YOLOX-L finds people (Apache-2.0) and dw-ll_ucoco_384 places 133
// whole-body keypoints (Apache-2.0, the IDEA-Research DWPose upload). Matte
// is MODNet (Apache-2.0). Upscale is Real-ESRGAN's compact x4 models
// (BSD-3-Clause). Interpolation is RIFE 4.9 (MIT).
var (
	yoloxL = modelFile{
		Name:   "yolox_l.onnx",
		URL:    "https://huggingface.co/yzd-v/DWPose/resolve/1a7144101628d69ee7a3768d1ee3a094070dc388/yolox_l.onnx",
		SHA256: "7860ae79de6c89a3c1eb72ae9a2756c0ccfbe04b7791bb5880afabd97855a411",
		Size:   216746733,
	}
	dwPose = modelFile{
		Name:   "dw-ll_ucoco_384.onnx",
		URL:    "https://huggingface.co/yzd-v/DWPose/resolve/1a7144101628d69ee7a3768d1ee3a094070dc388/dw-ll_ucoco_384.onnx",
		SHA256: "724f4ff2439ed61afb86fb8a1951ec39c6220682803b4a8bd4f598cd913b1843",
		Size:   134399116,
	}
	modnet = modelFile{
		Name:   "modnet.onnx",
		URL:    "https://huggingface.co/Xenova/modnet/resolve/fa2fa546052fba4c08921230a26cc69a333fca12/onnx/model.onnx",
		SHA256: "07c308cf0fc7e6e8b2065a12ed7fc07e1de8febb7dc7839d7b7f15dd66584df9",
		Size:   25888640,
	}
	esrganGeneral = modelFile{
		Name:   "realesr-general-x4v3.onnx",
		URL:    "https://huggingface.co/skillsafe-ai/realesr-general-x4v3/resolve/042a40bc4c918349ad3e2e607a68ae509a4c27b5/model.onnx",
		SHA256: "a946f7a9397021b9b6b7e71df3d2821b04cc09ff244423b7ca79cb191ce4a00e",
		Size:   4866428,
	}
	esrganAnime = modelFile{
		Name:   "realesr-animevideov3.onnx",
		URL:    "https://huggingface.co/skillsafe-ai/realesr-animevideov3/resolve/185e9142d439d17e3fb99395600fb7d08af09de5/model.onnx",
		SHA256: "78baa685a1a92cac6e14ab2af2a8ad0ef56fa48629563b1414e3a7acc54d86e2",
		Size:   2492908,
	}
	rife49 = modelFile{
		Name:   "rife_v4.9.onnx",
		URL:    "https://huggingface.co/FuryTMP/RIFE_v4.9/resolve/d30537a6f5e71f1baac6c2d5a03e74b650f916fe/rife_v4.9.onnx",
		SHA256: "b53f43b0853f73c055b1ba56dced48296d48a0ab76ce123bb7b492bf6c2ae57a",
		Size:   21367656,
	}
)

// modelFilesFor are the model files a tool other than depth needs at
// most. The runtime comes with them (runtimeFiles).
func modelFilesFor(id string) []modelFile {
	switch id {
	case "pose":
		return []modelFile{yoloxL, dwPose}
	case "matte":
		return []modelFile{modnet}
	case "upscale":
		return []modelFile{esrganGeneral}
	case "interp":
		return []modelFile{rife49}
	}
	return nil
}
