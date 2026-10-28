package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The clip tools are ffmpeg alone: no model, so they start at once.

// frameTool keeps one frame of a clip as a library image, to use as a
// reference or a long video's end image.
var frameTool = &tool{
	ID: "frame", Name: "Grab a frame", Code: "FR", Action: "Grab frame",
	Blurb: "One frame of a clip, kept as a library image at the clip's size",
	Takes: []string{kindVideo},
	plan: func(o toolOrder) ([]toolOut, error) {
		at, err := frameAt(o)
		if err != nil {
			return nil, err
		}
		return []toolOut{{Kind: kindImage, Label: fmt.Sprintf("%s · frame at %s", o.Source.Label, seconds(at))}}, nil
	},
	run: func(j *toolJob) error {
		at, err := frameAt(j.order)
		if err != nil {
			return err
		}
		j.note(true, "Grabbing the frame")
		dst := j.work(".png")
		if err := j.r.frames.encode(j.ctx, "-ss", fmt.Sprintf("%.3f", at), "-i", j.src(), "-frames:v", "1",
			"-c:v", "png", "-f", "image2", "-y", dst); err != nil {
			return err
		}
		return j.keep(0, dst)
	},
}

// frameAt is the order's time, in seconds, kept a frame inside the clip.
func frameAt(o toolOrder) (float64, error) {
	at, err := strconv.ParseFloat(o.Form.Get("at"), 64)
	if err != nil || math.IsNaN(at) {
		return 0, failedf("Set the time to grab")
	}
	return math.Round(min(max(at, 0), max(o.Source.Seconds-0.05, 0))*100) / 100, nil
}

// Conform choices: frame rate, short side, and shape, each "keep" first.
var (
	conformRates  = []string{"keep", "16", "24", "25", "30"}
	conformSizes  = []string{"keep", "480", "720", "1080"}
	conformShapes = []string{"keep", "portrait", "landscape", "square"}
	// conformCrops center-crop to a shape, as large as the frame allows.
	conformCrops = map[string]string{
		"portrait":  "crop='min(iw,ih*9/16)':'min(ih,iw*16/9)'",
		"landscape": "crop='min(iw,ih*16/9)':'min(ih,iw*9/16)'",
		"square":    "crop='min(iw,ih)':'min(iw,ih)'",
	}
	conformShapeNames = map[string]string{"portrait": "9:16", "landscape": "16:9", "square": "1:1"}
)

// conformTool remakes a clip at a frame rate, size, or shape, so a run
// gets what its model expects instead of the engine cropping for it.
var conformTool = &tool{
	ID: "conform", Name: "Conform", Code: "CF", Action: "Conform",
	Blurb: "A clip at another frame rate, size, or shape, its sound kept",
	Takes: []string{kindVideo},
	plan: func(o toolOrder) ([]toolOut, error) {
		rate, size, shape := conformChoices(o)
		var parts []string
		if rate != "keep" {
			parts = append(parts, rate+" fps")
		}
		if size != "keep" {
			parts = append(parts, size+"p")
		}
		if shape != "keep" {
			parts = append(parts, conformShapeNames[shape])
		}
		if len(parts) == 0 {
			return nil, failedf("Change the frame rate, size, or shape")
		}
		return []toolOut{{Kind: kindVideo, Label: o.Source.Label + " · " + strings.Join(parts, " ")}}, nil
	},
	run: func(j *toolJob) error {
		j.note(true, "Conforming")
		dst := j.work(".mp4")
		if err := j.r.frames.encode(j.ctx, "-i", j.src(), "-map", "0:v:0", "-map", "0:a?",
			"-vf", conformFilter(conformChoices(j.order)), "-c:v", "libx264", "-preset", "veryfast", "-crf", "14",
			"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-y", dst); err != nil {
			return err
		}
		return j.keep(0, dst)
	},
}

func conformChoices(o toolOrder) (rate, size, shape string) {
	return formChoice(o.Form, "rate", conformRates), formChoice(o.Form, "size", conformSizes), formChoice(o.Form, "shape", conformShapes)
}

// conformFilter crops to the shape, scales the short side, and sets the
// rate, ending on even sides, since H.264 needs them.
func conformFilter(rate, size, shape string) string {
	var f []string
	if c, ok := conformCrops[shape]; ok {
		f = append(f, c)
	}
	if size != "keep" {
		f = append(f, fmt.Sprintf("scale='if(lt(iw,ih),%s,-2)':'if(lt(iw,ih),-2,%s)':flags=bicubic", size, size))
	}
	f = append(f, "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	if rate != "keep" {
		f = append(f, "fps="+rate)
	}
	return strings.Join(f, ",")
}

// audioWAVSeconds is the longest sound kept as WAV: 48 kHz 16-bit stereo
// is 192 kB a second, and the library takes audio up to 30 MB. Longer
// sound is kept as MP3.
const audioWAVSeconds = 150

// audioTool keeps a clip's sound as library audio: a long video's
// soundtrack or reference audio.
var audioTool = &tool{
	ID: "audio", Name: "Extract audio", Code: "AU", Action: "Extract audio",
	Blurb: "A clip's sound, kept as library audio for a soundtrack or a reference",
	Takes: []string{kindVideo},
	plan: func(o toolOrder) ([]toolOut, error) {
		return []toolOut{{Kind: kindAudio, Label: o.Source.Label + " · sound"}}, nil
	},
	run: func(j *toolJob) error {
		if !j.r.frames.hasSound(j.ctx, j.src()) {
			return failedf("%s has no sound", j.order.Source.Label)
		}
		j.note(true, "Extracting the sound")
		dst, codec := j.work(".wav"), []string{"-c:a", "pcm_s16le", "-ar", "48000", "-f", "wav"}
		if j.order.Source.Seconds > audioWAVSeconds {
			dst, codec = j.work(".mp3"), []string{"-c:a", "libmp3lame", "-b:a", "192k", "-f", "mp3"}
		}
		args := append([]string{"-i", j.src(), "-vn", "-map", "0:a:0"}, codec...)
		if err := j.r.frames.encode(j.ctx, append(args, "-y", dst)...); err != nil {
			return err
		}
		return j.keep(0, dst)
	},
}
