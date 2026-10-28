package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tgzOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestArchivesKeepTheirMembersFlatInTheirFolder downloads a wheel and a
// tarball as the GPU runtime's are, from a local server.
func TestArchivesKeepTheirMembersFlatInTheirFolder(t *testing.T) {
	wheel := zipOf(t, map[string]string{"nvidia/cu13/lib/liba.so.13": "A", "nvidia/cu13/lib/libb.so.13": "B", "nvidia/other.txt": "x"})
	tarball := tgzOf(t, map[string]string{"ort/lib/libort.so.1": "O", "ort/include/x.h": "h"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := tarball
		if strings.HasSuffix(r.URL.Path, ".whl") {
			body = wheel
		}
		if _, err := w.Write(body); err != nil {
			// The client left; the test reports what it got.
			return
		}
	}))
	defer srv.Close()
	m := newModels(t.TempDir())
	whl := modelFile{Name: "liba.so.13", Dir: "cuda", URL: srv.URL + "/x.whl", SHA256: sum(wheel), Size: int64(len(wheel)),
		Members: []string{"nvidia/cu13/lib/liba.so.13", "nvidia/cu13/lib/libb.so.13"}}
	tgz := modelFile{Name: "libort.so.1", Dir: "cuda", URL: srv.URL + "/x.tgz", SHA256: sum(tarball), Size: int64(len(tarball)),
		Members: []string{"ort/lib/libort.so.1"}}
	if m.have(whl) || m.missing(whl, tgz) != int64(len(wheel)+len(tarball)) {
		t.Fatal("files read as present before any download")
	}
	if err := m.ensure(context.Background(), func(int64, int64) {}, whl, tgz); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"liba.so.13": "A", "libb.so.13": "B", "libort.so.1": "O"} {
		got, err := os.ReadFile(filepath.Join(m.dir, "cuda", name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if !m.have(whl) || !m.have(tgz) || m.path(tgz) != filepath.Join(m.dir, "cuda", "libort.so.1") {
		t.Error("kept files do not read as present where the code loads them")
	}
	if _, err := os.Stat(filepath.Join(m.dir, "cuda", "other.txt")); err == nil {
		t.Error("a member not asked for was kept")
	}

	bad := whl
	bad.Dir, bad.SHA256 = "bad", strings.Repeat("0", 64)
	if err := m.ensure(context.Background(), func(int64, int64) {}, bad); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Errorf("a wrong hash: %v", err)
	}
	gone := tgz
	gone.Dir, gone.Members = "gone", []string{"ort/lib/libmissing.so"}
	if err := m.ensure(context.Background(), func(int64, int64) {}, gone); err == nil || !strings.Contains(err.Error(), "has no ort/lib/libmissing.so") {
		t.Errorf("a missing member: %v", err)
	}
}

func TestRuntimeFilesFollowTheDevice(t *testing.T) {
	if got := runtimeFiles(toolDevice{Name: "CPU"}); len(got) != 1 || got[0].Name != ortRuntime.Name || got[0].Dir != "" {
		t.Errorf("CPU runtime = %+v", got)
	}
	gpu := runtimeFiles(toolDevice{GPU: true, Name: "GPU"})
	var names []string
	for _, f := range gpu {
		if f.Dir != "cuda" {
			t.Errorf("%s is not kept with the CUDA libraries", f.Name)
		}
		names = append(names, f.Name)
	}
	m := newModels("/m")
	for _, lib := range cudaPreload {
		found := false
		for _, f := range gpu {
			for _, p := range m.kept(f) {
				found = found || filepath.Base(p) == lib
			}
		}
		if !found {
			t.Errorf("%s is preloaded but no GPU file keeps it (%v)", lib, names)
		}
	}
}

func TestGPUFullKnowsTheProvidersOutOfMemory(t *testing.T) {
	// The failure a Stable depth job hit 2026-10-18 while a local ComfyUI
	// held 15.5 of the GPU's 20 GB.
	got := errors.New("depth head: Error running network: Non-zero status code returned while running Resize node. " +
		"Name:'/head/Resize' Status Message: /onnxruntime_src/onnxruntime/core/framework/bfc_arena.cc:360 " +
		"void* onnxruntime::BFCArena::AllocateRawInternal(size_t, bool, onnxruntime::Stream*) " +
		"Failed to allocate memory for requested buffer of size 1930772480.")
	if !gpuFull(got) || !gpuFull(errors.New("CUDA failure 2: out of memory")) {
		t.Error("an out-of-memory failure was not read as the GPU being full")
	}
	if gpuFull(errors.New("depth model: invalid input shape")) || gpuFull(nil) {
		t.Error("another failure was read as the GPU being full")
	}
}
