package main

import (
	"fmt"
	"strings"
)

// Each image model is prompted the way its makers document, so the same
// request reads differently per family. Sources, read 2026-10-17:
//
//   - qwen: the "Image Prompt Rewriting Expert" system prompt shipped in
//     ComfyUI's Qwen Image 2.1 templates. An observer's single paragraph that
//     opens "The image is a ⟨orientation⟩ ⟨style⟩ ⟨medium⟩ of …", places
//     things by position, gives lighting its own sentence, and closes with one
//     "The overall composition …" sentence. No quality boosters.
//   - qwen-edit: the Qwen Image 2.1 Edit template notes. An instruction that
//     names references as <image1>, <image2>.
//   - zimage: community guides for Z-Image Turbo, which runs without a
//     negative prompt. Image type and subject first, then layout, setting,
//     light, style, and constraints written as positives.
//   - krea2: the prompt-engineer system prompt in ComfyUI's Krea-2 template.
//     One faithful paragraph that ends with its exact no-text sentence.
//   - gemini: Google's Gemini image-generation guide (Nano Banana).
//     Hyper-specific, with the purpose stated, steps for complex frames,
//     camera language, and exclusions phrased positively; references by
//     order ("the first image").
//   - seedream: BytePlus's Seedream 4.0 to 5.0 prompt guide. Purpose and type
//     first ("Design a …"), coherent natural language, references as
//     "Image 1". "A set" or "a series" asks it for several images, so sheets
//     avoid those words.
//   - gpt: OpenAI's GPT Image prompting guide (OpenAI cookbook). A skimmable
//     template in a fixed order (scene, subject, key details, constraints)
//     with the intended use stated; inputs named by index and role
//     ("Image 1: …"); edits say "change only" and repeat what stays, with
//     "Do not redesign the character" for character sheets.
//   - krea3: ComfyUI's Krea 3 notes. Detailed sentences on subject, light, and
//     framing; edits say what changes and what stays; references as @Image1.

// sheetBrief is one request, before it is worded for a model.
type sheetBrief struct {
	Purpose string // character, environment, object, place, free
	Subject string // trimmed, without a trailing period
	Refs    int
}

// imagePrompt words a request for an engine's family. Anything ("free")
// is sent as typed.
func imagePrompt(e imageEngine, purpose, subject string, refs int) string {
	if purpose == "free" || purpose == "" {
		return strings.TrimSpace(subject)
	}
	b := sheetBrief{Purpose: purpose, Refs: refs,
		Subject: strings.TrimSpace(strings.TrimRight(strings.TrimSpace(subject), "."))}
	switch e.Family {
	case "flux":
		if b.Refs > 0 {
			return editPrompt(b, func(n int) string { return fmt.Sprintf("image %d", n) }, " Preserve the referenced subject's identity and materials.")
		}
		return "Create one finished image. " + zimagePrompt(b)
	case "qwen":
		return qwenPrompt(b)
	case "qwen-edit":
		return editPrompt(b, func(n int) string { return fmt.Sprintf("<image%d>", n) }, "")
	case "zimage":
		return zimagePrompt(b)
	case "krea2":
		return krea2Prompt(b)
	case "gemini":
		if b.Refs > 0 {
			return editPrompt(b, ordinalImage, " The result is a single finished image.")
		}
		return geminiPrompt(b)
	case "seedream":
		if b.Refs > 0 {
			return editPrompt(b, func(n int) string { return fmt.Sprintf("Image %d", n) }, "")
		}
		return seedreamPrompt(b)
	case "gpt":
		if b.Refs > 0 {
			return gptEditPrompt(b)
		}
		return gptPrompt(b)
	case "krea3":
		if b.Refs > 0 {
			return editPrompt(b, func(n int) string { return fmt.Sprintf("@Image%d", n) }, "")
		}
		return krea3Prompt(b)
	}
	return zimagePrompt(b)
}

func (b sheetBrief) or(fallback string) string {
	if b.Subject == "" {
		return fallback
	}
	return b.Subject
}

// The shared content of each sheet, worded per family below.
const (
	characterViews = "front view, three-quarter view, side profile, and back view"
	environmentRow = "a reverse angle, a close detail of the materials and surfaces, and a high-angle view of the layout"
)

func qwenPrompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "The image is a wide flat-lit character reference sheet of " + b.or("an original character") + ", shown four times " +
			"against a plain light gray backdrop. From left to right across the frame, the same figure stands at the same scale " +
			"with the feet on one shared baseline: on the far left a front view, then a three-quarter view, then a side profile " +
			"facing right, and on the far right a back view. Each figure holds a relaxed neutral standing pose with the arms " +
			"slightly away from the body, so the outline of the clothing reads clearly. The face, hairstyle, body proportions, " +
			"every garment, and every color match exactly from one view to the next, and the spacing between the four figures " +
			"is even. Along the top and bottom edges the backdrop stays empty, and the sheet carries no labels, captions, or " +
			"lettering. The lighting is soft, even studio light from the front, leaving only faint contact shadows under the " +
			"feet. The overall composition is a balanced, evenly spaced turnaround in a neutral palette that keeps attention " +
			"on the character's design."
	case "environment":
		return "The image is a wide concept-art environment sheet of " + b.or("an original location") + ", laid out as four " +
			"panels on a neutral gray ground with thin even gutters. Across the upper two-thirds of the frame, one wide " +
			"establishing view shows the whole space at eye level with a clear foreground, middle ground, and background. Along " +
			"the lower third, three smaller panels sit side by side: on the left a reverse angle of the same place, in the " +
			"centre a close detail of its materials and surfaces, and on the right a high-angle view of its layout. The " +
			"architecture, materials, palette, time of day, and light are identical in all four panels, and the place is empty " +
			"of people. No panel carries a label, caption, or lettering. The lighting is natural and consistent, with the same " +
			"direction and colour in every view. The overall composition reads as one coherent location seen four ways, in a " +
			"restrained, believable palette."
	case "object":
		return "The image is a square product reference photograph of " + b.or("a single object") + ", centred on a seamless " +
			"light gray background. The object sits entirely in frame with generous space on every side, seen from a " +
			"three-quarter angle slightly above, so its front, one side, and its top are all visible. Its materials, edges, and " +
			"surface texture are rendered in sharp detail, and nothing else appears in the frame: no hands, props, or lettering. " +
			"The lighting is soft, even studio light with a gentle shadow directly beneath the object. The overall composition " +
			"is clean and centred, in a neutral palette that leaves the object's own colours unchanged."
	default: // place
		return "The image is a wide eye-level establishing photograph of " + b.or("a place") + ". Across the frame the whole " +
			"space and its layout are visible, from a clear foreground at the bottom edge through the middle ground to the far " +
			"background at the top. The place is empty of people, and its materials and textures are shown at a realistic " +
			"scale. The lighting is natural and believable for the time of day. The overall composition is open and balanced, " +
			"with no visible lettering."
	}
}

func zimagePrompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "Character turnaround sheet of " + b.or("an original character") + ": the same character shown four times in " +
			"a row, full body from head to toe, " + characterViews + ", all at the same scale with feet on one baseline, " +
			"standing in a relaxed neutral pose with arms slightly away from the body. Identical face, hairstyle, proportions, " +
			"outfit, and colors in every view. Plain clean light gray studio backdrop with nothing else in the frame, soft even " +
			"front lighting, sharp focus, and a purely visual sheet free of any lettering."
	case "environment":
		return "Environment concept sheet of " + b.or("an original location") + ": one wide establishing view across the top " +
			"two-thirds, and below it three smaller panels of the same place, " + environmentRow + ". Same architecture, " +
			"materials, palette, time of day, and light in every panel, the place empty of people, thin even gray gutters " +
			"between panels, natural light, sharp focus, and a purely visual sheet free of any lettering."
	case "object":
		return "Product reference photo of " + b.or("a single object") + ", centered and whole in frame with space around it, " +
			"three-quarter view from slightly above, on a plain seamless light gray backdrop with nothing else in the frame, " +
			"soft even studio lighting, sharp detail on materials and edges, free of any lettering."
	default:
		return "Wide eye-level establishing shot of " + b.or("a place") + ", the whole space and its layout visible with a " +
			"clear foreground, middle ground, and background, empty of people, natural believable light, realistic scale and " +
			"materials, free of any lettering."
	}
}

// krea2NoText is the closing sentence Krea-2's own prompt engineer appends.
const krea2NoText = "Absolutely no text, no typography, no letters, no words, no numbers, no logos, no watermark, no captions, pure imagery only."

func krea2Prompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "A clean character turnaround sheet of " + b.or("an original character") + ", presented as four full-body " +
			"views in a single row, " + characterViews + ", each at the same scale with the feet resting on one baseline and " +
			"a relaxed neutral pose with the arms held slightly away from the body, the face, hairstyle, proportions, outfit, " +
			"and colors identical from view to view, set against a plain light gray studio backdrop under soft even light. " + krea2NoText
	case "environment":
		return "A concept-art environment sheet of " + b.or("an original location") + ", with one wide establishing view " +
			"filling the upper two-thirds and a row of three smaller panels beneath it showing " + environmentRow + ", the " +
			"architecture, materials, palette, time of day, and light kept the same in every panel, the space empty of people, " +
			"separated by thin neutral gutters. " + krea2NoText
	case "object":
		return "A product reference image of " + b.or("a single object") + ", centered and entirely in frame, seen from a " +
			"three-quarter angle slightly above on a plain seamless light gray backdrop, lit by soft even studio light with " +
			"crisp detail on its materials and edges, with nothing else in the frame. " + krea2NoText
	default:
		return "A wide eye-level establishing view of " + b.or("a place") + ", showing the whole space and its layout from " +
			"foreground to background, empty of people, in natural, believable light with realistic materials and scale. " + krea2NoText
	}
}

func geminiPrompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "Create a character turnaround sheet of " + b.or("an original character") + " for a video production, so " +
			"the character can be recast consistently from any angle. Show the same character four times in one row, full body " +
			"from head to toe: " + characterViews + ". Keep every figure at the same scale with the feet on one baseline, in a " +
			"relaxed neutral pose with the arms slightly away from the body. The face, hairstyle, proportions, outfit, and " +
			"colors are identical in all four views. Use a plain light gray studio backdrop and soft, even front lighting, " +
			"shot straight on at eye level with a long lens so the proportions stay true. The sheet is purely visual, with no " +
			"labels or captions."
	case "environment":
		return "Create an environment design sheet of " + b.or("an original location") + " for a video production, so the " +
			"same place can be filmed from several angles. First, fill the upper two-thirds with one wide establishing shot at " +
			"eye level. Then, along the bottom, add three smaller panels of the same place: " + environmentRow + ". Keep the " +
			"architecture, materials, palette, time of day, and lighting identical in every panel, with an empty, unpeopled " +
			"space throughout and thin neutral gutters between panels. The sheet is purely visual, with no labels or captions."
	case "object":
		return "A high-resolution, studio-lit product photograph of " + b.or("a single object") + " on a seamless light gray " +
			"background, made as a reference image. The lighting is a soft, even softbox setup that shows its true colors. The " +
			"camera angle is a three-quarter view from slightly above to show its front, side, and top, with the whole object in " +
			"frame and space around it. Ultra-realistic, with sharp focus on its materials and edges, and an otherwise empty frame."
	default:
		return "A photorealistic wide-angle establishing shot of " + b.or("a place") + ", made as a location reference. The " +
			"whole space and its layout are visible from a clear foreground to the far background, shot at eye level, with " +
			"natural, believable light and an empty, unpeopled scene."
	}
}

func seedreamPrompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "Design a character turnaround sheet in one image for " + b.or("an original character") + ". The sheet shows " +
			"the same character four times in a row, full body from head to toe: " + characterViews + ", all at the same " +
			"scale and standing in a relaxed neutral pose. The face, hairstyle, proportions, outfit, and colors are identical in " +
			"every view. Plain light gray background, soft even studio lighting. No text or labels."
	case "environment":
		return "Design an environment concept sheet in one image for " + b.or("an original location") + ". A wide " +
			"establishing view fills the upper two-thirds, and a row of three smaller panels below shows " + environmentRow +
			". The architecture, materials, colors, time of day, and lighting stay the same in every panel, the place is empty " +
			"of people, and thin gray gutters separate the panels. No text or labels."
	case "object":
		return "Design a product reference image of " + b.or("a single object") + ": the object stands centered and whole on " +
			"a seamless light gray background, seen from a three-quarter angle slightly above, under soft even studio lighting " +
			"with sharp detail on its materials. Nothing else in the frame. No text."
	default:
		return "A wide eye-level establishing shot of " + b.or("a place") + " for a location reference: the whole space and " +
			"its layout from foreground to background, empty of people, natural light, realistic materials. No text."
	}
}

func krea3Prompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "A character turnaround sheet of " + b.or("an original character") + ", four full-body views in one row (" +
			characterViews + ") at the same scale on a plain light gray studio backdrop, relaxed neutral pose, identical face, " +
			"hair, outfit, and colors in every view, lit by soft even front light and framed straight on with even spacing. No text."
	case "environment":
		return "An environment concept sheet of " + b.or("an original location") + ": a wide eye-level establishing view across " +
			"the top two-thirds and three smaller panels below (" + environmentRow + "), with the same architecture, materials, " +
			"palette, and light in every panel, empty of people, natural light, thin gray gutters. No text."
	case "object":
		return "A product reference photo of " + b.or("a single object") + ", centered and whole on a seamless light gray " +
			"backdrop, three-quarter view from slightly above, soft even studio light, sharp detail on materials and edges. No text."
	default:
		return "A wide eye-level establishing shot of " + b.or("a place") + ", the whole space visible from foreground to " +
			"background, empty of people, natural light, realistic materials and scale. No text."
	}
}

func gptPrompt(b sheetBrief) string {
	switch b.Purpose {
	case "character":
		return "Intended use: a character reference sheet for a video production.\n" +
			"Scene: a plain light gray studio backdrop with soft, even front lighting.\n" +
			"Subject: " + b.or("an original character") + ", shown four times in one row, full body from head to toe: " + characterViews + ".\n" +
			"Key details: the same scale with the feet on one baseline; a relaxed neutral pose with the arms slightly away from " +
			"the body; identical face, hairstyle, proportions, outfit, and colors in every view.\n" +
			"Constraints: no text, labels, or captions; nothing else in the frame."
	case "environment":
		return "Intended use: an environment design sheet for a video production.\n" +
			"Scene: " + b.or("an original location") + ", empty of people, in natural light.\n" +
			"Subject: one wide eye-level establishing view across the upper two-thirds, and a row of three smaller panels " +
			"below: " + environmentRow + ".\n" +
			"Key details: the same architecture, materials, palette, time of day, and light in every panel; thin neutral " +
			"gutters between panels.\n" +
			"Constraints: no text, labels, or captions."
	case "object":
		return "Intended use: a product reference image.\n" +
			"Scene: a seamless light gray backdrop with soft, even studio lighting.\n" +
			"Subject: " + b.or("a single object") + ", centered and entirely in frame, seen from a three-quarter angle slightly above.\n" +
			"Key details: photorealistic, with sharp detail on its materials, edges, and surface texture.\n" +
			"Constraints: no hands, props, or text; nothing else in the frame."
	default:
		return "Intended use: a location reference for a video production.\n" +
			"Scene: " + b.or("a place") + ", empty of people, in natural, believable light.\n" +
			"Subject: a photorealistic wide establishing shot at eye level, the whole space visible from foreground to background.\n" +
			"Constraints: no text or signage added."
	}
}

// gptEditPrompt names each input by index and role, says what changes,
// and repeats what stays, as OpenAI's guide does.
func gptEditPrompt(b sheetBrief) string {
	var inputs strings.Builder
	for i := 1; i <= b.Refs; i++ {
		role := "the reference"
		switch {
		case b.Purpose == "character" && i == 1:
			role = "the character to draw"
		case b.Purpose == "character":
			role = "more of the same character"
		case b.Purpose == "environment" || b.Purpose == "place":
			role = "the place"
		case b.Purpose == "object":
			role = "the object"
		}
		fmt.Fprintf(&inputs, "Image %d: %s.\n", i, role)
	}
	extra := ""
	if b.Subject != "" {
		extra = "\nNotes: " + b.Subject + "."
	}
	switch b.Purpose {
	case "character":
		return inputs.String() + "Make a character reference sheet of the character in Image 1 for a video production: the " +
			"same character four times in one row, full body from head to toe, " + characterViews + ", on a plain light gray " +
			"backdrop with soft, even lighting.\nKeep the face, hairstyle, body, outfit, and colors exactly the same in every " +
			"view. Do not redesign the character.\nConstraints: no text, labels, or captions." + extra
	case "environment":
		return inputs.String() + "Make an environment design sheet of the place in Image 1: one wide eye-level establishing " +
			"view across the upper two-thirds, and three smaller panels below: " + environmentRow + ".\nKeep its architecture, " +
			"materials, colors, and light the same in every panel, empty of people.\nConstraints: no text, labels, or captions." + extra
	case "object":
		return inputs.String() + "Change only the setting: place " + b.or("the main object") + " from Image 1 alone, centered " +
			"and whole, on a seamless light gray backdrop with soft, even studio lighting, three-quarter view.\nKeep its shape, " +
			"colors, materials, and any labels exactly the same. Do not restyle the object.\nConstraints: nothing else in the frame." + extra
	default:
		return inputs.String() + "Change only the framing: show the place in Image 1 as a wide eye-level establishing shot, " +
			"empty of people.\nKeep its architecture, materials, colors, and light the same.\nConstraints: no text added." + extra
	}
}

// editPrompt words a request that starts from references, naming them
// with ref(n) the way the model expects. The last note is appended as is.
func editPrompt(b sheetBrief, ref func(int) string, note string) string {
	which := ref(1)
	switch {
	case b.Refs == 2:
		which = ref(1) + " and " + ref(2)
	case b.Refs > 2:
		names := make([]string, b.Refs)
		for i := range names {
			names[i] = ref(i + 1)
		}
		which = strings.Join(names[:b.Refs-1], ", ") + ", and " + names[b.Refs-1]
	}
	extra := ""
	if b.Subject != "" {
		extra = " " + b.Subject + "."
	}
	var p string
	switch b.Purpose {
	case "character":
		p = "Make a character turnaround sheet of the character in " + which + ". Show them four times in one row, full body " +
			"from head to toe: " + characterViews + ", at the same scale, in a relaxed neutral pose. Keep their face, " +
			"hairstyle, body, outfit, and colors exactly as in " + which + "." + extra + " Plain light gray background, soft " +
			"even studio lighting, and no text or labels."
	case "environment":
		p = "Make an environment concept sheet of the place in " + which + ": one wide establishing view across the top " +
			"two-thirds, and three smaller panels below showing " + environmentRow + ". Keep its architecture, materials, " +
			"colors, and light exactly as in " + which + ", empty of people." + extra + " Thin gray gutters between panels, and " +
			"no text or labels."
	case "object":
		p = "Isolate " + b.or("the main object") + " from " + which + " as a clean reference image: the same object, " +
			"unchanged in shape, color, and material, centered and whole, three-quarter view, on a plain seamless light gray " +
			"background with soft even studio lighting. Remove everything else, and add no text."
	default:
		p = "Show the setting from " + which + " as a clean wide establishing shot at eye level: keep its architecture, " +
			"materials, colors, and light, empty of people." + extra + " No text."
	}
	return p + note
}

// ordinalImage names a reference by order, as Google's guide does.
func ordinalImage(n int) string {
	words := []string{"", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth",
		"eleventh", "twelfth", "thirteenth", "fourteenth"}
	if n < len(words) {
		return "the " + words[n] + " image"
	}
	return fmt.Sprintf("image %d", n)
}
