package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// Audio references are WAV or MP3, the two formats Wan's reference audio
// takes. Like mp4.go, their length is read from the file itself, so no
// ffprobe is needed.

// errAudioFormat is a file that is neither a readable WAV nor an MP3.
var errAudioFormat = errors.New("not a readable WAV or MP3")

// sniffMP3 reports an MP3 that starts with a frame instead of an ID3 tag,
// which http.DetectContentType does not recognize.
func sniffMP3(head []byte) bool {
	_, ok := mp3Frame(head)
	return ok
}

// audioSeconds is the length of a WAV or MP3 of size bytes.
func audioSeconds(r io.ReaderAt, size int64, ctype string) (float64, error) {
	if ctype == "audio/wav" {
		return wavSeconds(r, size)
	}
	return mp3Seconds(r, size)
}

// wavSeconds reads the fmt chunk's byte rate and the data chunk's size.
func wavSeconds(r io.ReaderAt, size int64) (float64, error) {
	var head [12]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return 0, errAudioFormat
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0, errAudioFormat
	}
	var byteRate uint32
	for off := int64(12); off+8 <= size; {
		var ch [8]byte
		if _, err := r.ReadAt(ch[:], off); err != nil {
			return 0, errAudioFormat
		}
		n := int64(binary.LittleEndian.Uint32(ch[4:8]))
		body := off + 8
		switch string(ch[0:4]) {
		case "fmt ":
			var f [12]byte
			if _, err := r.ReadAt(f[:], body); err != nil {
				return 0, errAudioFormat
			}
			byteRate = binary.LittleEndian.Uint32(f[8:12])
		case "data":
			if byteRate == 0 {
				return 0, errAudioFormat
			}
			// A streamed WAV leaves the size unset; the rest of the file is data.
			if n == 0 || n == 0xFFFFFFFF || body+n > size {
				n = size - body
			}
			return float64(n) / float64(byteRate), nil
		}
		off = body + n + n%2
	}
	return 0, errAudioFormat
}

// mp3Header is one MPEG audio frame header's facts.
type mp3Header struct {
	at         int // offset of the frame in the buffer it was found in
	bitrate    int // bits per second
	sampleRate int
	samples    int  // per frame
	mpeg1      bool // MPEG-1, as against MPEG-2 or 2.5
	mono       bool
}

var (
	mp3Rates1 = [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mp3Rates2 = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	mp3Hz     = map[int][3]int{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}
)

// mp3Frame finds the first valid Layer III frame header in b.
func mp3Frame(b []byte) (mp3Header, bool) {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] != 0xFF || b[i+1]&0xE0 != 0xE0 {
			continue
		}
		version := int(b[i+1]>>3) & 3
		layer := int(b[i+1]>>1) & 3
		rateIdx := int(b[i+2] >> 4)
		hzIdx := int(b[i+2]>>2) & 3
		if version == 1 || layer != 1 || rateIdx == 0 || rateIdx == 15 || hzIdx == 3 {
			continue
		}
		h := mp3Header{at: i, sampleRate: mp3Hz[version][hzIdx], mpeg1: version == 3, mono: b[i+3]>>6 == 3}
		if h.mpeg1 {
			h.bitrate, h.samples = mp3Rates1[rateIdx]*1000, 1152
		} else {
			h.bitrate, h.samples = mp3Rates2[rateIdx]*1000, 576
		}
		return h, true
	}
	return mp3Header{}, false
}

// mp3Seconds skips an ID3v2 tag, then reads the frame count from a Xing,
// Info, or VBRI header, or, without one, assumes a constant bitrate.
func mp3Seconds(r io.ReaderAt, size int64) (float64, error) {
	start := int64(0)
	var id3 [10]byte
	if _, err := r.ReadAt(id3[:], 0); err == nil && string(id3[0:3]) == "ID3" {
		tag := int64(id3[6]&0x7F)<<21 | int64(id3[7]&0x7F)<<14 | int64(id3[8]&0x7F)<<7 | int64(id3[9]&0x7F)
		start = 10 + tag
		if id3[5]&0x10 != 0 {
			start += 10
		}
	}
	buf := make([]byte, 64<<10)
	n, err := r.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	buf = buf[:n]
	h, ok := mp3Frame(buf)
	if !ok {
		return 0, errAudioFormat
	}
	side := 17
	switch {
	case h.mpeg1 && !h.mono:
		side = 32
	case !h.mpeg1 && h.mono:
		side = 9
	}
	if x := h.at + 4 + side; x+12 <= len(buf) {
		tag := buf[x : x+4]
		if bytes.Equal(tag, []byte("Xing")) || bytes.Equal(tag, []byte("Info")) {
			if flags := binary.BigEndian.Uint32(buf[x+4 : x+8]); flags&1 != 0 {
				frames := binary.BigEndian.Uint32(buf[x+8 : x+12])
				return float64(frames) * float64(h.samples) / float64(h.sampleRate), nil
			}
		}
	}
	if v := h.at + 4 + 32; v+18 <= len(buf) && bytes.Equal(buf[v:v+4], []byte("VBRI")) {
		frames := binary.BigEndian.Uint32(buf[v+14 : v+18])
		return float64(frames) * float64(h.samples) / float64(h.sampleRate), nil
	}
	audio := size - start - int64(h.at)
	if audio <= 0 {
		return 0, errAudioFormat
	}
	return float64(audio) * 8 / float64(h.bitrate), nil
}
