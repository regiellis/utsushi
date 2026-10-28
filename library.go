package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig reads the sizes of these formats
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

// Accepted media: what the engines take as source video and references.
// MP4 only for video, and WAV or MP3 for audio, because their durations can
// be read without ffprobe. Keys are the sniffed type; ctype is what is
// stored and served.
var mediaTypes = map[string]struct {
	kind, ext, ctype string
	max              int64
}{
	"image/jpeg": {kindImage, ".jpg", "image/jpeg", 20 << 20},
	"image/png":  {kindImage, ".png", "image/png", 20 << 20},
	"image/webp": {kindImage, ".webp", "image/webp", 20 << 20},
	"video/mp4":  {kindVideo, ".mp4", "video/mp4", 200 << 20},
	"audio/wave": {kindAudio, ".wav", "audio/wav", 30 << 20},
	"audio/mpeg": {kindAudio, ".mp3", "audio/mpeg", 30 << 20},
}

// maxResultBytes bounds one downloaded generation.
const maxResultBytes = 2 << 30

// errMediaType is a file whose sniffed type is not accepted.
var errMediaType = errors.New("only JPEG, PNG, WebP images, MP4 videos, and WAV or MP3 audio are accepted")

// errNoLength is an MP4 whose movie header cannot be read.
var errNoLength = errors.New("the video's length cannot be read")

// errTooLarge is a file over its type's limit.
type errTooLarge struct{ limit int64 }

func (e errTooLarge) Error() string {
	return fmt.Sprintf("the file is larger than %d MB", e.limit>>20)
}

// library stores media files under one directory. Names are random, chosen
// here, and written through an os.Root, so no input ever picks a path.
type library struct {
	dir  string
	root *os.Root
	web  *http.Client
}

func openLibrary(dir string) (*library, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	// Result URLs are public signed links; no credential goes with them.
	web := &http.Client{Timeout: 15 * time.Minute}
	return &library{dir: dir, root: root, web: web}, nil
}

func (l *library) Close() error { return l.root.Close() }

func (l *library) path(m Media) string { return filepath.Join(l.dir, m.File) }

func (l *library) open(m Media) (*os.File, error) {
	if m.File == "" {
		return nil, errNotFound
	}
	return l.root.Open(m.File)
}

func (l *library) remove(m Media) error {
	if m.File == "" {
		return nil
	}
	err := l.root.Remove(m.File)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// store writes r as a new media file. The type is sniffed from the bytes, not
// taken from a client or a server header. limit, when positive, caps the
// size below the type's own limit.
func (l *library) store(r io.Reader, origin, label string, limit int64) (Media, error) {
	br := bufio.NewReaderSize(r, 4096)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return Media{}, err
	}
	ctype := http.DetectContentType(head)
	if ctype == "application/octet-stream" && sniffMP3(head) {
		ctype = "audio/mpeg"
	}
	t, ok := mediaTypes[ctype]
	if !ok {
		return Media{}, errMediaType
	}
	max := t.max
	if origin == originResult {
		max = maxResultBytes
	}
	if limit > 0 {
		max = min(max, limit)
	}
	m := Media{
		ID: newID(), Kind: t.kind, Origin: origin, ContentType: t.ctype,
		Label: clip(label, 120), Status: mediaReady, CreatedAt: now(),
	}
	m.File = m.ID + t.ext
	f, err := l.root.OpenFile(m.File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return Media{}, err
	}
	n, err := io.Copy(f, io.LimitReader(br, max+1))
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = errTooLarge{max}
	}
	if err == nil && t.kind == kindVideo {
		m.Seconds, err = l.seconds(m.File)
	}
	if err == nil && t.kind == kindAudio {
		m.Seconds, err = l.audioLength(m.File, n, t.ctype)
	}
	if err == nil {
		m.Width, m.Height = l.size(m)
	}
	if err != nil {
		if rerr := l.root.Remove(m.File); rerr != nil {
			return Media{}, errors.Join(err, rerr)
		}
		return Media{}, err
	}
	m.Size = n
	return m, nil
}

func (l *library) seconds(name string) (_ float64, err error) {
	f, err := l.root.Open(name)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	s, err := mp4Seconds(f)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errNoLength, err)
	}
	return s, nil
}

func (l *library) audioLength(name string, size int64, ctype string) (_ float64, err error) {
	f, err := l.root.Open(name)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	s, err := audioSeconds(f, size, ctype)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errNoLength, err)
	}
	return s, nil
}

// fetch downloads a generation's signed URL into the library. Partner links
// expire (Runway's within hours, Wan's in a day), so a result is kept here.
func (l *library) fetch(ctx context.Context, rawURL, label string) (_ Media, err error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return Media{}, fmt.Errorf("result URL is not https: %q", clip(rawURL, 60))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Media{}, err
	}
	resp, err := l.web.Do(req)
	if err != nil {
		return Media{}, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return Media{}, fmt.Errorf("result download answered %s", resp.Status)
	}
	return l.store(resp.Body, originResult, label, 0)
}

// size reads a stored file's frame or image size, 0 by 0 when it cannot.
// A missing size only costs the card its native shape, so it never fails a
// store.
func (l *library) size(m Media) (int, int) {
	f, err := l.root.Open(m.File)
	if err != nil {
		return 0, 0
	}
	defer func() {
		// Read-only: a close error loses nothing.
		if cerr := f.Close(); cerr != nil {
			log.Printf("utsushi: close %s: %v", m.File, cerr)
		}
	}()
	if m.Kind == kindAudio {
		return 0, 0
	}
	if m.Kind == kindVideo {
		w, h, err := mp4Size(f)
		if err != nil {
			return 0, 0
		}
		return w, h
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
