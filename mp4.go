package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// mp4Seconds reads a video's duration from its movie header (moov/mvhd).
// The engines bound source length (Wan 2.7 edits 2 to 10 seconds, Runway and
// Genjutsu take up to 30), and reading one box keeps ffprobe out of the app.
// The header can sit after the media data, so boxes are skipped, not read.
func mp4Seconds(r io.ReadSeeker) (float64, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	moovStart, moovEnd, err := findBox(r, 0, end, "moov")
	if err != nil {
		return 0, err
	}
	hdrStart, hdrEnd, err := findBox(r, moovStart, moovEnd, "mvhd")
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(hdrStart, io.SeekStart); err != nil {
		return 0, err
	}
	body := make([]byte, min(hdrEnd-hdrStart, 32))
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, fmt.Errorf("mvhd: %w", err)
	}
	var scale uint32
	var length uint64
	switch body[0] {
	case 0:
		if len(body) < 20 {
			return 0, errors.New("mvhd is too short")
		}
		scale = binary.BigEndian.Uint32(body[12:16])
		length = uint64(binary.BigEndian.Uint32(body[16:20]))
	case 1:
		if len(body) < 32 {
			return 0, errors.New("mvhd is too short")
		}
		scale = binary.BigEndian.Uint32(body[20:24])
		length = binary.BigEndian.Uint64(body[24:32])
	default:
		return 0, fmt.Errorf("mvhd version %d is unknown", body[0])
	}
	if scale == 0 {
		return 0, errors.New("mvhd has a zero timescale")
	}
	return float64(length) / float64(scale), nil
}

// mp4Size reads the coded frame size of the first video track from its track
// header (moov/trak/tkhd). Rotation in the track matrix is not applied: the
// size is the one decoded frames have.
func mp4Size(r io.ReadSeeker) (int, int, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}
	moovStart, moovEnd, err := findBox(r, 0, end, "moov")
	if err != nil {
		return 0, 0, err
	}
	var w, h int
	found := false
	err = scanBoxes(r, moovStart, moovEnd, func(typ string, start, end int64) (bool, error) {
		if typ != "trak" {
			return false, nil
		}
		video, err := isVideoTrack(r, start, end)
		if err != nil || !video {
			return false, err
		}
		if w, h, err = trackSize(r, start, end); err != nil {
			return false, err
		}
		found = true
		return true, nil
	})
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, errors.New("no video track")
	}
	return w, h, nil
}

// isVideoTrack reads a trak's mdia/hdlr handler type.
func isVideoTrack(r io.ReadSeeker, start, end int64) (bool, error) {
	mdiaStart, mdiaEnd, err := findBox(r, start, end, "mdia")
	if err != nil {
		return false, err
	}
	hdlrStart, hdlrEnd, err := findBox(r, mdiaStart, mdiaEnd, "hdlr")
	if err != nil {
		return false, err
	}
	body, err := readBody(r, hdlrStart, hdlrEnd, 12)
	if err != nil {
		return false, fmt.Errorf("hdlr: %w", err)
	}
	return string(body[8:12]) == "vide", nil
}

// trackSize reads width and height, 16.16 fixed point, from a trak's tkhd.
func trackSize(r io.ReadSeeker, start, end int64) (int, int, error) {
	hdrStart, hdrEnd, err := findBox(r, start, end, "tkhd")
	if err != nil {
		return 0, 0, err
	}
	body, err := readBody(r, hdrStart, hdrEnd, 96)
	if err != nil && !errors.Is(err, errShortBox) {
		return 0, 0, fmt.Errorf("tkhd: %w", err)
	}
	at := 76
	if len(body) > 0 && body[0] == 1 {
		at = 88
	}
	if len(body) < at+8 {
		return 0, 0, errors.New("tkhd is too short")
	}
	w := int(binary.BigEndian.Uint32(body[at:at+4]) >> 16)
	h := int(binary.BigEndian.Uint32(body[at+4:at+8]) >> 16)
	if w == 0 || h == 0 {
		return 0, 0, errors.New("tkhd has no frame size")
	}
	return w, h, nil
}

var errShortBox = errors.New("box is shorter than expected")

// readBody reads up to n bytes of a box body. It returns what it read with
// errShortBox when the body is shorter than n.
func readBody(r io.ReadSeeker, start, end int64, n int) ([]byte, error) {
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	body := make([]byte, min(end-start, int64(n)))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	if len(body) < n {
		return body, errShortBox
	}
	return body, nil
}

// findBox scans the boxes in [start, end) for one of type name and returns
// the byte range of its body.
func findBox(r io.ReadSeeker, start, end int64, name string) (int64, int64, error) {
	var bodyStart, bodyEnd int64
	found := false
	err := scanBoxes(r, start, end, func(typ string, s, e int64) (bool, error) {
		if typ == name {
			bodyStart, bodyEnd, found = s, e, true
		}
		return found, nil
	})
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, fmt.Errorf("no %s box", name)
	}
	return bodyStart, bodyEnd, nil
}

// scanBoxes calls fn with each box in [start, end) and the byte range of its
// body, until fn reports it is done.
func scanBoxes(r io.ReadSeeker, start, end int64, fn func(typ string, start, end int64) (bool, error)) error {
	pos := start
	hdr := make([]byte, 16)
	for pos+8 <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		head := int64(8)
		switch size {
		case 0: // the box runs to the end of its parent
			size = end - pos
		case 1: // a 64-bit size follows the type
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return err
			}
			large := binary.BigEndian.Uint64(hdr[8:16])
			if large > uint64(end-pos) {
				return fmt.Errorf("box %q overruns its parent", typ)
			}
			size = int64(large)
			head = 16
		}
		if size < head || pos+size > end {
			return fmt.Errorf("box %q has a bad size", typ)
		}
		done, err := fn(typ, pos+head, pos+size)
		if err != nil || done {
			return err
		}
		pos += size
	}
	return nil
}
