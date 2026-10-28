package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// wavFile is a PCM WAV of the given length, 8 kHz mono 16-bit.
func wavFile(seconds float64) []byte {
	const rate, block = 8000, 2
	data := make([]byte, int(seconds*rate)*block)
	var b bytes.Buffer
	b.WriteString("RIFF")
	write := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	write(uint32(36 + len(data)))
	b.WriteString("WAVEfmt ")
	write(uint32(16))
	write(uint16(1))
	write(uint16(1))
	write(uint32(rate))
	write(uint32(rate * block))
	write(uint16(block))
	write(uint16(16))
	b.WriteString("LIST")
	write(uint32(4))
	b.WriteString("INFO")
	b.WriteString("data")
	write(uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// mp3File is a constant-bitrate MPEG-1 Layer III stream: 128 kbit/s,
// 44.1 kHz, stereo, frames of 417 bytes. id3 prepends an ID3v2 tag.
func mp3File(frames int, id3 bool) []byte {
	var b bytes.Buffer
	if id3 {
		b.Write([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 20})
		b.Write(make([]byte, 20))
	}
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
	for range frames {
		b.Write(frame)
	}
	return b.Bytes()
}

func TestAudioLengthIsReadFromTheFile(t *testing.T) {
	wav := wavFile(2.5)
	got, err := audioSeconds(bytes.NewReader(wav), int64(len(wav)), "audio/wav")
	if err != nil || math.Abs(got-2.5) > 0.001 {
		t.Errorf("WAV reads %.3f s (%v), want 2.5", got, err)
	}
	for _, id3 := range []bool{false, true} {
		mp3 := mp3File(383, id3) // 383 frames of 1152 samples at 44.1 kHz is about 10 s
		got, err := audioSeconds(bytes.NewReader(mp3), int64(len(mp3)), "audio/mpeg")
		want := 383 * 417 * 8 / 128000.0
		if err != nil || math.Abs(got-want) > 0.05 {
			t.Errorf("MP3 (id3 %v) reads %.3f s (%v), want %.3f", id3, got, err, want)
		}
	}
	if _, err := audioSeconds(strings.NewReader("not audio at all, just text"), 27, "audio/mpeg"); err == nil {
		t.Error("text read as an MP3")
	}
}

func TestLibraryStoresAudioWithItsLength(t *testing.T) {
	lib, err := openLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lib.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, c := range []struct {
		name, ctype string
		data        []byte
	}{
		{"voice.wav", "audio/wav", wavFile(3)},
		{"beat.mp3", "audio/mpeg", mp3File(100, true)},
		{"raw.mp3", "audio/mpeg", mp3File(100, false)},
	} {
		m, err := lib.store(bytes.NewReader(c.data), originUpload, c.name, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m.Kind != kindAudio || m.ContentType != c.ctype || m.Seconds <= 0 {
			t.Errorf("%s stored as %s %s, %.2f s", c.name, m.Kind, m.ContentType, m.Seconds)
		}
	}
}

func TestValidateCountsEachReferenceKind(t *testing.T) {
	l := newWan3Engine(nil, nil, nil).info().Limits
	clip := Media{ID: "c", Kind: kindVideo, Status: mediaReady, Seconds: 6}
	img := Media{ID: "i", Kind: kindImage, Status: mediaReady}
	vid := func(s float64) Media { return Media{ID: "v", Kind: kindVideo, Status: mediaReady, Seconds: s} }
	aud := func(s float64) Media { return Media{ID: "a", Kind: kindAudio, Status: mediaReady, Seconds: s} }
	base := task{Source: clip, Resolution: "480P"}
	for _, c := range []struct {
		name string
		refs []Media
		want string // a substring of the refs error, empty for none
	}{
		{"image, video, and audio", []Media{img, vid(4), aud(5)}, ""},
		{"no image", []Media{vid(4)}, "at least 1 reference image"},
		{"clip and videos over 15 s", []Media{img, vid(5), vid(5)}, "Together the clip and reference videos may run 15.0 s"},
		{"audio over 15 s", []Media{img, aud(9), aud(9)}, "Together the audio clips may run 15.0 s"},
		{"six audio clips", []Media{img, aud(1), aud(1), aud(1), aud(1), aud(1), aud(1)}, "at most 5 audio clips"},
	} {
		tk := base
		tk.Refs = c.refs
		msgs := strings.Join(validate(l, tk)["refs"], " | ")
		if (c.want == "") != (msgs == "") || !strings.Contains(msgs, c.want) {
			t.Errorf("%s: refs errors %q, want %q", c.name, msgs, c.want)
		}
	}
	genjutsu := newGenjutsuEngine(nil, nil).info().Limits
	tk := task{Source: Media{Kind: kindVideo, Status: mediaReady, Seconds: 6}, Resolution: "720p", Refs: []Media{img, aud(3)}}
	if msgs := strings.Join(validate(genjutsu, tk)["refs"], " "); !strings.Contains(msgs, "takes no audio clips") {
		t.Errorf("Genjutsu took an audio reference: %q", msgs)
	}
}

func TestRefTagsNumberEachKindOnItsOwn(t *testing.T) {
	got := strings.Join(refTags([]Media{{Kind: kindImage}, {Kind: kindVideo}, {Kind: kindAudio}, {Kind: kindImage}, {Kind: kindVideo}}), " ")
	if got != "@1 @v1 @a1 @2 @v2" {
		t.Errorf("tags %q", got)
	}
}
