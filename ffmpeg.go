package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// frames uses ffmpeg, when the server has it, to pull still frames from
// clips: the preview cache (a poster per video and a strip of scrub frames)
// and the frames "Suggest from clip" shows the language model. Without
// ffmpeg, posters fall back to the video itself and suggestions use
// ComfyUI's first frame.
type frames struct {
	bin   string // empty without ffmpeg
	probe string // ffprobe, empty without it
	cache string // directory of cached previews

	mu sync.Mutex // one ffmpeg per file at a time
}

func newFrames(cacheDir string) (*frames, error) {
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		bin = ""
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		probe = ""
	}
	return &frames{bin: bin, probe: probe, cache: cacheDir}, nil
}

func (f *frames) available() bool { return f.bin != "" }

// version is ffmpeg's first version line, for About and diagnostics.
func (f *frames) version(ctx context.Context) string {
	if f.bin == "" {
		return "not installed"
	}
	out, err := exec.CommandContext(ctx, f.bin, "-hide_banner", "-version").Output()
	if err != nil {
		return "installed, version unreadable"
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimPrefix(line, "ffmpeg version ")
}

// run executes ffmpeg and returns its stdout.
func (f *frames) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// encode runs ffmpeg for a job that writes its own output file: cutting a
// clip into parts and joining them. Re-encoding thirty seconds takes far
// longer than a still, so it gets its own timeout.
func (f *frames) encode(ctx context.Context, args ...string) error {
	if f.bin == "" {
		return errOffline{"ffmpeg is not installed on the server."}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, clip(strings.TrimSpace(errOut.String()), 300))
	}
	return nil
}

// rate is a video's average frame rate as ffprobe reads it ("30000/1001").
func (f *frames) rate(ctx context.Context, video string) (string, error) {
	if f.probe == "" {
		return "", errOffline{"ffprobe is not installed on the server."}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, f.probe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=avg_frame_rate,r_frame_rate", "-of", "default=nw=1", video).Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe: %w", err)
	}
	return pickRate(string(out))
}

// pickRate reads ffprobe's key=value lines, which come in the stream's
// field order whatever order was asked for. The average rate wins: a phone
// clip can read r_frame_rate 60/1 over 24 frames a second, and encoding at
// that would play it 2.5 times too fast. A rate of 0/0 is none.
func pickRate(out string) (string, error) {
	rates := map[string]string{}
	for _, line := range strings.Fields(out) {
		if k, v, ok := strings.Cut(line, "="); ok {
			rates[k] = v
		}
	}
	for _, k := range []string{"avg_frame_rate", "r_frame_rate"} {
		r := rates[k]
		if num, den, ok := strings.Cut(r, "/"); ok && num != "0" && den != "0" {
			return r, nil
		}
	}
	return "", fmt.Errorf("ffprobe found no frame rate in %q", strings.TrimSpace(out))
}

// rawFrames decodes a video, or an image as one frame, to RGB frames of w
// by h, every decoded frame as it is, and calls each with them in order.
// The slice is reused between calls.
func (f *frames) rawFrames(ctx context.Context, path string, w, h int, each func([]byte) error) error {
	if f.bin == "" {
		return errOffline{"ffmpeg is not installed on the server."}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, "-hide_banner", "-loglevel", "error", "-i", path, "-an", "-sn",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=bicubic", w, h), "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	buf := make([]byte, w*h*3)
	var readErr error
	for {
		if _, err := io.ReadFull(out, buf); err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = fmt.Errorf("read a frame: %w", err)
			}
			break
		}
		if err := each(buf); err != nil {
			readErr = err
			break
		}
	}
	if readErr != nil {
		// Stop ffmpeg rather than wait for a decode nobody reads.
		cancel()
		return errors.Join(readErr, ignoreKilled(cmd.Wait()))
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, clip(strings.TrimSpace(errOut.String()), 300))
	}
	return nil
}

// ignoreKilled drops the error of a process this side stopped.
func ignoreKilled(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// rawVideo describes frames written to encodeRaw.
type rawVideo struct {
	W, H   int
	Format string // "gray" or "rgb24"
	Rate   string // ffmpeg's frame rate, "30000/1001"
	// OutW, OutH scale the frames on the way out.
	OutW, OutH int
	// Sound, when set, is a file whose audio the video carries.
	Sound string
	// Image writes one PNG instead of an MP4.
	Image bool
}

// encodeRaw has ffmpeg encode the frames write sends it to dst: an H.264
// MP4, or a PNG for an image.
func (f *frames) encodeRaw(ctx context.Context, dst string, v rawVideo, write func(io.Writer) error) error {
	if f.bin == "" {
		return errOffline{"ffmpeg is not installed on the server."}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", v.Format, "-s", fmt.Sprintf("%dx%d", v.W, v.H), "-framerate", v.Rate, "-i", "pipe:0"}
	scale := fmt.Sprintf("scale=%d:%d:flags=bicubic", v.OutW, v.OutH)
	if v.Image {
		args = append(args, "-vf", scale, "-frames:v", "1", "-c:v", "png", "-f", "image2", "-y", dst)
	} else {
		if v.Sound != "" {
			args = append(args, "-i", v.Sound, "-map", "0:v:0", "-map", "1:a?", "-c:a", "aac", "-b:a", "192k")
		}
		args = append(args, "-vf", scale, "-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p",
			"-movflags", "+faststart", "-f", "mp4", "-y", dst)
	}
	cmd := exec.CommandContext(ctx, f.bin, args...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := write(in)
	if cerr := in.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		cancel()
		return errors.Join(werr, ignoreKilled(cmd.Wait()))
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, clip(strings.TrimSpace(errOut.String()), 300))
	}
	return nil
}

// cutout multiplies a clip or image by its matte, so the people keep their
// colors over black. The matte is the source's size, made even for a
// video, so a video source is made even the same way first. Both start at
// time 0 and the output runs at the source's rate: the matte was encoded
// from raw frames with its own time base, and blend otherwise mixes the
// two inputs' timestamps (a 24 fps clip came out at 2880/119).
func (f *frames) cutout(ctx context.Context, src, matte, dst, rate string, image bool) error {
	even := "scale=trunc(iw/2)*2:trunc(ih/2)*2,"
	if image {
		even = ""
	}
	graph := "[0:v]" + even + "setpts=PTS-STARTPTS,format=gbrp[s];[1:v]setpts=PTS-STARTPTS,format=gbrp[m];[s][m]blend=all_mode=multiply"
	args := []string{"-i", src, "-i", matte, "-filter_complex", graph + ",format=rgb24"}
	if image {
		args = append(args, "-frames:v", "1", "-c:v", "png", "-f", "image2", "-y", dst)
	} else {
		args = []string{"-i", src, "-i", matte, "-filter_complex", graph + ",format=yuv420p[v]", "-map", "[v]", "-map", "0:a?",
			"-fps_mode", "cfr", "-r", rate, "-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-f", "mp4", "-y", dst}
	}
	return f.encode(ctx, args...)
}

// cuts are the times, in seconds, where a clip cuts from one shot to
// another (ffmpeg's scene score over 0.3).
func (f *frames) cuts(ctx context.Context, video string) ([]float64, error) {
	if f.bin == "" {
		return nil, errOffline{"ffmpeg is not installed on the server."}
	}
	out, err := f.run(ctx, "-i", video, "-an", "-vf", "select='gt(scene,0.3)',metadata=print:file=-", "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	var at []float64
	for _, field := range strings.Fields(string(out)) {
		v, ok := strings.CutPrefix(field, "pts_time:")
		if !ok {
			continue
		}
		t, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("read a cut time %q: %w", v, err)
		}
		at = append(at, t)
	}
	return at, nil
}

// hasSound reports whether a file has an audio stream ffmpeg can read.
func (f *frames) hasSound(ctx context.Context, path string) bool {
	if f.bin == "" {
		return false
	}
	_, err := f.run(ctx, "-i", path, "-map", "0:a:0", "-t", "0.05", "-f", "null", "-")
	return err == nil
}

// still returns one JPEG frame at at seconds, scaled to width.
func (f *frames) still(ctx context.Context, video string, at float64, width int) ([]byte, error) {
	if f.bin == "" {
		return nil, errOffline{"ffmpeg is not installed on the server."}
	}
	return f.run(ctx, "-ss", fmt.Sprintf("%.2f", at), "-i", video, "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width), "-f", "image2", "-c:v", "mjpeg", "-q:v", "4", "pipe:1")
}

// sample returns n frames spread across a clip of the given length.
func (f *frames) sample(ctx context.Context, video string, seconds float64, n int) ([][]byte, error) {
	var out [][]byte
	for i := range n {
		at := seconds * (float64(i) + 0.5) / float64(n)
		img, err := f.still(ctx, video, at, 512)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

// poster is a cached JPEG of a video's first moment, made on first request.
func (f *frames) poster(ctx context.Context, id, video string) (string, error) {
	return f.cached(ctx, id+"-poster.jpg", func() ([]byte, error) { return f.still(ctx, video, 0.1, 480) })
}

// strip is a cached row of ten frames across a video, for scrub previews.
func (f *frames) strip(ctx context.Context, id, video string, seconds float64) (string, error) {
	return f.cached(ctx, id+"-strip.jpg", func() ([]byte, error) {
		if f.bin == "" {
			return nil, errOffline{"ffmpeg is not installed on the server."}
		}
		fps := 10 / max(seconds, 0.1)
		return f.run(ctx, "-i", video, "-vf", fmt.Sprintf("fps=%.4f,scale=160:-2,tile=10x1", fps),
			"-frames:v", "1", "-f", "image2", "-c:v", "mjpeg", "-q:v", "5", "pipe:1")
	})
}

func (f *frames) cached(_ context.Context, name string, render func() ([]byte, error)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := filepath.Join(f.cache, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	data, err := render()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", err
	}
	return path, nil
}

// size is the cache's total size in bytes.
func (f *frames) size() (int64, error) {
	var total int64
	err := filepath.WalkDir(f.cache, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// clear empties the cache; previews are made again as they are needed.
func (f *frames) clear() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.cache)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.Remove(filepath.Join(f.cache, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// forget drops a media item's cached previews, when it is deleted.
func (f *frames) forget(id string) error {
	var errs []error
	for _, suffix := range []string{"-poster.jpg", "-strip.jpg"} {
		if err := os.Remove(filepath.Join(f.cache, id+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
