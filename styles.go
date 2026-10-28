package main

import (
	"fmt"
	"strings"
)

// stylePreset is one look in Style mode. Look describes it in words; the
// same words restyle the clip's first frame locally and guide the engine.
// The names follow the presets the owner listed from Higgsfield's Restyle;
// the descriptions are this app's own, since Higgsfield publishes none.
type stylePreset struct {
	Key, Name, Look string
}

var stylePresets = []stylePreset{
	{"hand-painted-cel", "Hand-Painted Cel Animation", "hand-painted cel animation, flat painted cels with clean ink outlines over soft watercolor backgrounds, a classic 2D feature-film look"},
	{"knitted-yarn", "Knitted Yarn World", "a knitted yarn world where every surface, person, and prop is hand-knitted wool and crochet with chunky visible stitches and soft fuzzy fibers"},
	{"expressive-2d", "Expressive 2D Animation", "expressive 2D animation with loose energetic linework, bold simplified shapes, and flat vivid colors"},
	{"rounded-toy-cgi", "Rounded Toy CGI", "rounded toy CGI, smooth plastic toy figures with soft rounded forms, glossy materials, and bright playful colors under studio lighting"},
	{"felt-puppet", "Felt Puppet Stop Motion", "felt puppet stop motion, handmade felt puppets and props with visible fabric fibers and stitching on a miniature set"},
	{"grotesque-street-cgi", "Grotesque Street CGI", "grotesque street CGI, exaggerated caricature 3D characters with oversized features, grimy urban textures, and harsh streetlight"},
	{"warm-painterly-3d", "Warm Painterly 3D Animation", "warm painterly 3D animation, stylized 3D forms with brushstroke textures, soft painterly shading, and warm golden light"},
	{"graphic-cel", "Graphic Cel", "graphic cel animation with hard-edged flat color shapes, strong silhouettes, a limited palette, and bold black accents"},
	{"gouache-fashion", "Expressive Gouache Fashion", "expressive gouache fashion illustration, opaque brush strokes and elongated fashion-sketch figures on textured paper"},
	{"sculptural-3d-fashion", "Sculptural 3D Fashion", "sculptural 3D fashion, sleek sculpted figures like high-end mannequins in smooth matte materials under editorial studio light"},
	{"chunky-low-poly", "Chunky Textured Low-Poly", "chunky textured low-poly 3D, blocky faceted geometry with hand-painted textures and visible polygons"},
	{"cel-shaded-anime", "Cel-Shaded CG Anime", "cel-shaded CG anime, 3D characters rendered with anime cel shading, crisp outlines, two-tone shadows, and vibrant color"},
	{"painterly-cinematic-3d", "Painterly Cinematic 3D", "painterly cinematic 3D, cinematic lighting and depth with painted brush textures, rich color grading, and atmospheric haze"},
	{"outline-free-cel", "Outline-free Cel", "outline-free cel animation, flat color shapes with no line art, soft tonal shading, and a clean minimal design"},
	{"painterly-sculpted", "Painterly Sculpted Animation", "painterly sculpted animation, characters that look sculpted from clay and painted by hand, with visible brush and tool marks"},
	{"neon-graphic-3d", "Neon Graphic Cinematic 3D", "neon graphic cinematic 3D, stylized forms with glowing neon rim light in saturated magenta and cyan against a dark night"},
	{"gouache-character", "Textured Gouache Character Illustration", "textured gouache character illustration, a storybook painting with grainy paper texture, soft matte colors, and hand-drawn characters"},
	{"comic-3d", "Comic 3D", "comic-book 3D, renders with bold ink outlines, halftone dot shading, and punchy saturated color"},
	{"retro-game-3d", "Retro Game 3D", "late-1990s retro game 3D, low-resolution textures, simple geometry, vertex lighting, and slight dithering"},
}

func presetByKey(key string) (stylePreset, bool) {
	for _, p := range stylePresets {
		if p.Key == key {
			return p, true
		}
	}
	return stylePreset{}, false
}

// frameInstruction is the Qwen Image Edit instruction that restyles a clip's
// first frame. It names what must stay, since an edit model otherwise
// drifts the people and the framing.
func frameInstruction(look string) string {
	return "Restyle <image1> completely as " + look + ". Every surface, every person, their hair and " +
		"clothes, and the background take this style. Keep the same people, faces, poses, framing, " +
		"composition, and camera angle exactly."
}

// swatchPrompt is the Z-Image prompt for a preset's sample tile. Every tile
// shows the same scene, so the tiles compare the styles and nothing else.
func swatchPrompt(look string) string {
	return "A dancer mid-move on a city street at dusk, full body, centered, rendered as " + look + "."
}

// stylePrompt tells an engine to restyle the clip like the style frame, the
// first reference. A user's own words follow. chars is how many character
// references come after the style frame; tag formats a reference number the
// way the engine reads it, or is nil when the engine takes no tags.
func stylePrompt(look, user string, chars int, tag func(int) string) string {
	var b strings.Builder
	style := "the look of the first reference image"
	if tag != nil {
		style = "the look of " + tag(1)
	}
	if look != "" {
		fmt.Fprintf(&b, "Restyle the whole video as %s, matching %s. ", look, style)
	} else {
		fmt.Fprintf(&b, "Restyle the whole video in the visual style of %s. ", style)
	}
	b.WriteString("Keep the motion, camera movement, framing, composition, and timing exactly as filmed.")
	if chars > 0 {
		names := make([]string, chars)
		for i := range names {
			if tag != nil {
				names[i] = tag(i + 2)
			} else {
				names[i] = fmt.Sprintf("reference %d", i+2)
			}
		}
		fmt.Fprintf(&b, " Cast the people as the characters in %s, drawn in the same style.", strings.Join(names, ", "))
	}
	if user = strings.TrimSpace(user); user != "" {
		b.WriteString(" " + user)
	}
	return b.String()
}
