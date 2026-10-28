package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/png"
	"path/filepath"
	"sync"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

// newTestStore opens a fresh database under t.TempDir.
func newTestStore(t *testing.T) *store {
	t.Helper()
	cfg := database.DefaultDBConfig()
	cfg.Path = filepath.Join(t.TempDir(), "test.db")
	db, err := database.NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	st, err := newStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newTestLibrary(t *testing.T) *library {
	t.Helper()
	lib, err := openLibrary(filepath.Join(t.TempDir(), "media"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lib.Close(); err != nil {
			t.Error(err)
		}
	})
	return lib
}

// box is one MP4 box with its 32-bit size header.
func box(typ string, body ...[]byte) []byte {
	payload := bytes.Join(body, nil)
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// mvhd0 is a version 0 movie header with a timescale and duration.
func mvhd0(scale, length uint32) []byte {
	b := make([]byte, 100)
	binary.BigEndian.PutUint32(b[12:], scale)
	binary.BigEndian.PutUint32(b[16:], length)
	return box("mvhd", b)
}

// trak is a track with a version 0 header of the given frame size and a
// handler of the given type ("vide", "soun").
func trak(handler string, width, height uint32) []byte {
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:], width<<16)
	binary.BigEndian.PutUint32(tkhd[80:], height<<16)
	hdlr := make([]byte, 24)
	copy(hdlr[8:], handler)
	return box("trak", box("tkhd", tkhd), box("mdia", box("hdlr", hdlr)))
}

// fakeMP4 is a minimal 1280x720 file the sniffer reads as video/mp4, with
// the movie header after the media data, as many encoders write it. Its
// audio track comes first, so readers must pick the video one.
func fakeMP4(seconds uint32) []byte {
	ftyp := box("ftyp", []byte("isom"), []byte{0, 0, 2, 0}, []byte("isomiso2mp41"))
	mdat := box("mdat", make([]byte, 2048))
	moov := box("moov", mvhd0(1000, seconds*1000), trak("soun", 0, 0), trak("vide", 1280, 720))
	return bytes.Join([][]byte{ftyp, mdat, moov}, nil)
}

func fakePNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// recorder is a publisher that keeps every pushed state.
type recorder struct {
	mu     sync.Mutex
	runs   []Run
	medias []Media
}

func (r *recorder) run(run Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, run)
}

func (r *recorder) media(m Media) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.medias = append(r.medias, m)
}

func (r *recorder) statuses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, run := range r.runs {
		if len(out) == 0 || out[len(out)-1] != run.Status {
			out = append(out, run.Status)
		}
	}
	return out
}

// staticURLs hands out fixed public URLs for library files.
type staticURLs struct {
	mu   sync.Mutex
	seen []string
}

func (s *staticURLs) publicURL(_ context.Context, m Media) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, m.ID)
	return "https://cdn.example.test/" + m.File, nil
}
