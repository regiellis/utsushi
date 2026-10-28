package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/regiellis/hyperchi/hyperchi"
	"golang.org/x/image/draw"
)

func (w *web) previewRoutes() {
	w.app.Get("/media/{id}/poster", w.preview(false))
	w.app.Get("/media/{id}/strip", w.preview(true))
}

// preview serves a video's cached poster or scrub strip, made with ffmpeg on
// first request. Without ffmpeg it is a 404, and pages fall back to the
// video itself.
func (w *web) preview(strip bool) hyperchi.Handler {
	return func(c *hyperchi.Context) error {
		ctx := c.Ctx()
		m, err := w.st.media(ctx, c.Param("id"))
		if errors.Is(err, errNotFound) || (err == nil && (m.Kind != kindVideo || m.Status != mediaReady)) {
			return hyperchi.NotFound("No such video.")
		}
		if err != nil {
			return err
		}
		if !w.frames.available() {
			return hyperchi.NotFound("ffmpeg is not installed on the server.")
		}
		var path string
		if strip {
			path, err = w.frames.strip(ctx, m.ID, w.lib.path(m), m.Seconds)
		} else {
			path, err = w.frames.poster(ctx, m.ID, w.lib.path(m))
		}
		if err != nil {
			return err
		}
		c.Response().Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(c.Response(), c.Request(), path)
		return nil
	}
}

func (w *web) llmRoutes() {
	w.app.Post("/llm/improve", w.llmWrite("improve"))
	w.app.Post("/llm/describe", w.llmDescribe)
	w.app.Post("/llm/suggest", w.llmWrite("suggest"))
}

type llmAsk struct {
	Prompt string   `form:"prompt"`
	Engine string   `form:"engine"`
	Mode   string   `form:"mode"`
	Source string   `form:"source"`
	Refs   []string `form:"refs"`
	Style  string   `form:"style"`
	// A long video's shot: the prompt of the shot before it, and whether
	// this one "continue"s it or "cut"s to a new shot.
	Before string `form:"before"`
	Join   string `form:"join"`
}

// llmReply answers a feature request: the new prompt, or why there is none.
func llmReply(c *hyperchi.Context, prompt string, err error) error {
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(hyperchi.H{"error": describe(err)})
	}
	return c.JSON(hyperchi.H{"prompt": prompt})
}

// llmFor is the model, when Settings turn the feature on.
func (w *web) llmFor(feature string) (*llmClient, error) {
	p := w.prefs.get().LLM
	on := map[string]bool{"improve": p.Improve, "describe": p.Describe, "suggest": p.Suggest}[feature]
	if !on {
		return nil, errOffline{"Turn this on under Settings, Language model."}
	}
	return newLLM(p)
}

func (w *web) engineBrief(id, mode string) string {
	name := "the video model"
	if en, ok := w.eng.get(id); ok {
		name = en.info().Name
	}
	switch mode {
	case modeSwap:
		return name + ", which replaces one thing in a clip and keeps the rest"
	case modeStyle:
		return name + ", which restyles a whole clip and keeps its motion and framing"
	case modeLong:
		return name + ", which makes a long video shot by shot from a prompt per shot and a cast of reference images"
	}
	if id == "wan-animate2" {
		return name + ", which makes a reference character perform a clip's motion. Its prompt " +
			"has two parts, \"Character Description: ...\" then \"Background description: ...\", and " +
			"describes looks only, never motion"
	}
	return name + ", which recasts the people in a clip and keeps its motion and camera"
}

var tagPattern = regexp.MustCompile(`@\w+`)

func tags(s string) []string {
	t := tagPattern.FindAllString(s, -1)
	slices.Sort(t)
	return slices.Compact(t)
}

// llmDescribe writes the character description a reference image shows.
func (w *web) llmDescribe(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[llmAsk](c)
	if err != nil {
		return err
	}
	if len(in.Refs) == 0 {
		return llmReply(c, "", errOffline{"Pick a cast reference to describe."})
	}
	client, err := w.llmFor("describe")
	if err != nil {
		return llmReply(c, "", err)
	}
	system, user, img, err := w.describeAsk(c.Ctx(), in)
	if err != nil {
		return llmReply(c, "", err)
	}
	ctx, cancel := context.WithTimeout(c.Ctx(), 90*time.Second)
	defer cancel()
	out, err := client.chat(ctx, system, user, []llmImage{img})
	return llmReply(c, strings.TrimSpace(out), err)
}

// describeAsk is what Describe sends the model: the first reference, a
// character sheet as its front figure (whole, a sheet's faces are a few
// pixels high and skin tone was misread), at 768 px. The eval sends this too.
func (w *web) describeAsk(ctx context.Context, in llmAsk) (string, string, llmImage, error) {
	m, err := w.st.media(ctx, in.Refs[0])
	if err != nil {
		return "", "", llmImage{}, err
	}
	data, err := w.castImage(m, true, 768)
	if err != nil {
		return "", "", llmImage{}, err
	}
	system := "You describe a character reference image for a video prompt."
	user := "Describe the character in this image: face, skin tone, hair, build, clothing, colors, and art style, as " +
		"the image shows them. Refer to them as @1. Two sentences. Output only the description."
	if engineModelID(in.Engine) == "wan-animate2" {
		user = "Write exactly two lines. First: \"Character Description: \" then the character's looks " +
			"(face, skin tone, hair, build, clothing, colors, art style). Second: \"Background description: \" then a " +
			"fitting background. Looks only, no motion. Output only the two lines."
	}
	return system, user, llmImage{Type: "image/jpeg", Data: data}, nil
}

// llmScene is what the language model sees of the studio: frames of the
// clip, each picked reference, and the style, with a note saying which
// image is which and which tags the prompt may use.
type llmScene struct {
	images  []llmImage
	layout  string
	allowed []string // tags the prompt may use, sorted
	need    []string // tags the prompt must keep
	tags    bool     // the engine reads @tags
	// some is true when the prompt must name at least one reference by
	// tag but not all: a long video's shot may feature part of its cast.
	some bool
	// people is true where the references are people (Motion Transfer, long
	// videos), so describing a sheet's views is refused; elsewhere "rear
	// view" is just a camera angle on a car.
	people bool
	// controls are the control videos among the clip and the references (a
	// depth map, a pose), by how the prompt refers to each.
	controls []sceneControl
}

// sceneControl is a control video in the scene and how the prompt refers
// to it: its tag, or "the source clip" for an engine without tags.
type sceneControl struct {
	ref string
	controlClip
}

// scene gathers the clip frames, references, and style for a prompt.
func (w *web) scene(ctx context.Context, in llmAsk) (llmScene, error) {
	sc := llmScene{people: in.Mode == modeMotion || in.Mode == modeLong}
	var info engineInfo
	if en, ok := w.eng.get(in.Engine); ok {
		info = en.info()
	}
	sc.tags = info.Tags
	var notes []string
	if in.Source != "" {
		src, err := w.st.media(ctx, in.Source)
		if err != nil {
			return sc, err
		}
		stills, err := w.clipFrames(ctx, src)
		if err != nil {
			return sc, err
		}
		// In Motion Transfer the clip's people are replaced; saying so on
		// each frame keeps a small vision model from describing them. A
		// control clip says what it is instead: its frames are no footage.
		use := ""
		cc, control := controlOf(src)
		switch {
		case control:
			use = " (not footage: " + cc.Shows + ")"
		case in.Mode == modeMotion && len(in.Refs) > 0:
			use = " (for motion, camera, and setting only; the people in it are replaced by the references)"
		case in.Mode == modeLong:
			use = " (the video this shot continues; it ends on the last frame)"
		}
		for i, s := range stills {
			sc.images = append(sc.images, llmImage{Type: "image/jpeg", Data: s, Label: fmt.Sprintf("Clip frame %d of %d%s:", i+1, len(stills), use)})
		}
		clip := "the source clip"
		if sc.tags {
			clip += " (the prompt calls it @clip)"
			sc.allowed = append(sc.allowed, "@clip")
		}
		notes = append(notes, fmt.Sprintf("The clip frames are from %s, in order, %.1f s long.", clip, src.Seconds))
		if control {
			ref := "the source clip"
			if sc.tags {
				ref = "@clip"
			}
			sc.controls = append(sc.controls, sceneControl{ref, cc})
		}
	}
	clipFrames := len(sc.images)
	refs, err := w.st.mediaByIDs(ctx, in.Refs)
	if err != nil {
		return sc, err
	}
	tagged := refTags(refs)
	for i, m := range refs {
		name := "Reference " + strings.TrimPrefix(tagged[i], "@")
		if sc.tags {
			name = "Reference " + tagged[i]
			sc.allowed = append(sc.allowed, tagged[i])
			// A long video's shot may feature only part of its cast.
			if in.Mode != modeLong {
				sc.need = append(sc.need, tagged[i])
			} else if m.Kind == kindImage {
				sc.some = true
			}
		}
		switch m.Kind {
		case kindAudio:
			// A chat model cannot hear it; say what it is.
			notes = append(notes, fmt.Sprintf("%s is an audio clip, %q, %.1f s long, to use for voice, music, or sound.", name, m.Label, m.Seconds))
		case kindVideo:
			if !w.frames.available() {
				notes = append(notes, fmt.Sprintf("%s is a %.1f s video (no frame could be taken).", name, m.Seconds))
				continue
			}
			still, err := w.frames.still(ctx, w.lib.path(m), m.Seconds/2, 512)
			if err != nil {
				return sc, err
			}
			label := fmt.Sprintf("%s (a frame from the middle of a %.1f s video):", name, m.Seconds)
			if cc, ok := controlOf(m); ok {
				label = fmt.Sprintf("%s (a frame from the middle of a %.1f s %s control video, not footage: %s):", name, m.Seconds, cc.Name, cc.Shows)
				ref := "the reference video"
				if sc.tags {
					ref = tagged[i]
				}
				sc.controls = append(sc.controls, sceneControl{ref, cc})
			}
			sc.images = append(sc.images, llmImage{Type: "image/jpeg", Data: still, Label: label})
		default:
			img, err := w.castImage(m, in.Mode == modeLong, 512)
			if err != nil {
				return sc, err
			}
			sc.images = append(sc.images, llmImage{Type: "image/jpeg", Data: img, Label: name + ":"})
		}
	}
	if len(refs) > 0 {
		order := "follow the clip frames"
		if in.Mode == modeMotion {
			// The people come from the references, so the model sees them
			// first; it describes what it sees first most closely.
			sc.images = append(slices.Clone(sc.images[clipFrames:]), sc.images[:clipFrames]...)
			order = "come before the clip frames"
		}
		notes = append(notes, fmt.Sprintf("%d reference%s %s, each labeled.", len(refs), plural(len(refs)), order))
		if info.Limits.RefsFor != "" {
			notes = append(notes, "The engine uses references as: "+info.Limits.RefsFor)
		}
	}
	if id, ok := strings.CutPrefix(in.Style, "image:"); ok {
		m, err := w.st.media(ctx, id)
		if err != nil {
			return sc, err
		}
		img, err := w.shrink(m, 512)
		if err != nil {
			return sc, err
		}
		sc.images = append(sc.images, llmImage{Type: "image/jpeg", Data: img, Label: "The style every frame should take:"})
		notes = append(notes, "The last image is the style every frame should take.")
	} else if p, ok := presetByKey(in.Style); ok {
		notes = append(notes, "Every frame should take this style: "+p.Name+". "+p.Look)
	}
	slices.Sort(sc.allowed)
	sc.layout = strings.Join(notes, " ")
	return sc, nil
}

// rules are the scene's instructions about naming references, and about
// any control video in it.
func (sc llmScene) rules() string {
	r := sc.tagRules()
	for _, c := range sc.controls {
		r += " " + c.guide(c.ref)
	}
	return r
}

func (sc llmScene) tagRules() string {
	if sc.tags {
		r := "Name each reference by its tag where it belongs, and add a few words on what it shows. " +
			"Images are @1, @2; videos @v1, @v2; audio @a1, @a2."
		if len(sc.allowed) > 0 {
			r += " Use only these tags: " + strings.Join(sc.allowed, ", ") + "."
		}
		return r
	}
	return "This engine sees the reference images but cannot read tags, so describe each reference by " +
		"what it shows (face, hair, build, clothing, colors) and never write @ tags."
}

// sheetWords are how a reply describes a character sheet's layout instead
// of the person on it; a video model may take "front, side, and back views"
// literally.
var sheetWords = regexp.MustCompile(`(?i)\b(three-quarter|character sheets?|turnarounds?|(front|side|back|rear|profile) views?|(several|four|multiple) (views|angles|poses))\b`)

// check says what a reply broke, or is empty.
func (sc llmScene) check(out string) string {
	for _, c := range sc.controls {
		if w := c.look.FindString(out); w != "" {
			return "it describes how the " + c.Name + " control video looks (" + w + "); a video model would draw that. " +
				"Describe the people from their references and the setting with real light and colors"
		}
		if c.Kind != "" && !strings.Contains(strings.ToLower(out), c.Kind) {
			return "it never tells the video model that " + c.ref + " is its " + c.Name + "; say that the video follows the " +
				c.Name + " of " + c.ref + " for the motion and layout"
		}
	}
	if w := sheetWords.FindString(out); w != "" && sc.people {
		return "it describes a reference image's layout (" + w + "); describe each person once, as they look, and never the image's views or poses"
	}
	got := tags(out)
	if !sc.tags {
		if len(got) > 0 {
			return "it used tags (" + strings.Join(got, ", ") + ") this engine cannot read; describe the references instead"
		}
		return ""
	}
	for _, t := range got {
		if !slices.Contains(sc.allowed, t) {
			return "it used " + t + ", which names no picked reference; use only " + strings.Join(sc.allowed, ", ")
		}
	}
	for _, t := range sc.need {
		if !slices.Contains(got, t) {
			return "it left out " + t + "; every picked reference must appear by its tag"
		}
	}
	if sc.some && len(got) == 0 {
		return "it names no one by tag; name each person in the shot by their tag (" + strings.Join(sc.allowed, ", ") + ")"
	}
	return ""
}

// llmWrite drafts a prompt (suggest) or rewrites the visitor's (improve)
// from what the clip, references, and style show. A reply that misuses
// the tags is sent back once with the reason, then refused.
func (w *web) llmWrite(feature string) hyperchi.Handler {
	return func(c *hyperchi.Context) error {
		in, err := hyperchi.Bind[llmAsk](c)
		if err != nil {
			return err
		}
		draft := strings.TrimSpace(in.Prompt)
		if feature == "improve" && draft == "" {
			return llmReply(c, "", errOffline{"Write a rough prompt first, then improve it."})
		}
		if feature == "suggest" && in.Source == "" && in.Mode != modeLong {
			return llmReply(c, "", errOffline{"Pick a source clip first."})
		}
		client, err := w.llmFor(feature)
		if err != nil {
			return llmReply(c, "", err)
		}
		ctx, cancel := context.WithTimeout(c.Ctx(), 150*time.Second)
		defer cancel()
		system, user, sc, err := w.promptAsk(ctx, feature, in)
		if err != nil {
			return llmReply(c, "", err)
		}
		out, err := writePrompt(ctx, client, system, user, sc)
		return llmReply(c, out, err)
	}
}

// writePrompt asks the model for a prompt. A reply that misuses the tags is
// sent back once with the reason, then refused.
func writePrompt(ctx context.Context, client *llmClient, system, user string, sc llmScene) (string, error) {
	out, err := client.chat(ctx, system, user, sc.images)
	if err != nil {
		return "", err
	}
	out = strings.Trim(out, "\"` \n")
	if problem := sc.check(out); problem != "" {
		retry := user + "\n\nYour last answer was:\n" + out + "\n\nIt cannot be used because " + problem + ". Answer again."
		if out, err = client.chat(ctx, system, retry, sc.images); err != nil {
			return "", err
		}
		out = strings.Trim(out, "\"` \n")
		if problem := sc.check(out); problem != "" {
			return "", failedf("The model's prompt was not usable (%s), so yours was kept", problem)
		}
	}
	return out, nil
}

// sources says which image a prompt takes each detail from, per mode. A
// video model draws whatever the prompt describes, so a prompt that
// describes the clip's people (their shirts, chains, hair) pulls the result
// back toward them; the prompt eval (task utsushi:eval) grades exactly that.
func sources(mode string) string {
	switch mode {
	case modeSwap:
		return "The clip gives everything except the one thing being replaced. Describe the replacement in detail from its " +
			"reference, say where it sits in the clip, and describe the rest of the clip only briefly."
	case modeStyle:
		return "The clip gives the motion, framing, and what happens; the style gives the look of every frame. Describe the " +
			"people and setting only as shapes and actions, and the look from the style."
	case modeLong:
		return "You write one shot of a longer video. Each person in it looks entirely like their reference: face, skin " +
			"tone, hair, build, clothing, and accessories. Name each person by their tag and describe them once from their " +
			"reference image, skin tone included, as it is in the image. A reference image may show one person several times from different angles: describe that person once, " +
			"never the image, its angles, or its poses. Say how many people are in the shot and that no one else is in it, " +
			"because the model adds strangers to wide shots otherwise. Then say what happens, where, the light, and how " +
			"the camera moves."
	}
	return "The clip gives only the motion, timing, camera, framing, and setting. Each recast person looks entirely like " +
		"their reference: face, hair, build, clothing, and accessories. Describe each person from their reference image and " +
		"never from the clip: leave out the clip's people's clothes, hair, accessories, and skin, because the video model " +
		"draws whatever the prompt describes. A reference image may show one person several times from different angles: " +
		"describe that person once, never the image, its angles, or its poses. Match each reference to a performer by position or " +
		"role (the performer on the left becomes the first reference), and say that anyone shown alone in a shot is still " +
		"their recast self, so no one from the clip remains. Describe the setting, light, and camera from the clip."
}

// promptAsk is what Suggest (feature "suggest") and Improve ("improve")
// send the language model: the system and user messages, and the scene
// whose images go with them. The prompt eval sends exactly this.
func (w *web) promptAsk(ctx context.Context, feature string, in llmAsk) (string, string, llmScene, error) {
	sc, err := w.scene(ctx, in)
	if err != nil {
		return "", "", sc, err
	}
	length := "Write one paragraph under 120 words that says what should be seen: subject, look, setting, light, camera."
	if engineModelID(in.Engine) == "wan-animate2" {
		length = "Keep the two-part format and describe looks only."
	}
	system := "You write prompts for " + w.engineBrief(in.Engine, in.Mode) + ". " + sources(in.Mode) + " " +
		sc.rules() + " " + length + " Output only the prompt."
	user := sc.layout + "\n\n"
	if in.Mode == modeLong {
		switch {
		case strings.TrimSpace(in.Before) == "":
			user += "This is the video's first shot.\n\n"
		case in.Join == "continue":
			user += "The shot before, which this one continues with no cut (keep the place, light, and outfits, and carry " +
				"the action on):\n" + strings.TrimSpace(in.Before) + "\n\n"
		default:
			user += "The shot before, which this one cuts away from to a new shot:\n" + strings.TrimSpace(in.Before) + "\n\n"
		}
	}
	if feature == "improve" {
		user += "The visitor's rough prompt:\n" + strings.TrimSpace(in.Prompt) + "\n\nRewrite it for the engine, keeping its intent and " +
			"adding what the images show. Where it describes how the clip's people look, describe the references instead, " +
			"unless it asks outright to keep something from the clip."
	} else {
		if in.Mode == modeLong {
			user += "Write this shot's prompt for this cast. Begin each person with their tag, as in \"@1, a woman " +
				"with...\", described as their reference shows them; then say what happens, where, the light, and the camera."
		} else {
			user += "Write a starting prompt for this clip and these references."
		}
		if in.Mode == modeStyle {
			// Without this, half the drafts described the scene well and
			// never said how it should look.
			user += " End by saying how every frame looks, in the style's own words."
		}
		if in.Mode == modeMotion && len(in.Refs) > 0 {
			who := "each recast person"
			if sc.tags {
				who += ", named by their tag,"
			}
			if len(sc.controls) > 0 {
				// A control clip shows no light or colors, so the setting
				// cannot come from it as it does from footage.
				c := sc.controls[0]
				user += " Start with " + who + " as their reference shows them, then where they are with real light and colors, " +
					"then say the video follows the " + c.Name + " of " + c.ref + " for the motion and layout."
			} else {
				user += " Start with " + who + " as their reference shows them, then the setting, light, and camera from " +
					"the clip. Do not describe the people in the clip."
			}
		}
	}
	return system, user, sc, nil
}

// castImage is a reference image as the model sees it, at 512 px. For a
// long video a character sheet is its front figure, as the video model gets
// it: a whole four-view sheet at 512 px leaves each face a few pixels high,
// and the model misread skin tone from it.
func (w *web) castImage(m Media, front bool, px int) ([]byte, error) {
	if !front || w.frames == nil {
		return w.shrink(m, px)
	}
	path, err := frontView(w.lib, w.frames.cache, m)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return shrinkBytes(data, px)
}

// clipFrames are three frames across a clip with ffmpeg, or the first frame
// from ComfyUI without it.
func (w *web) clipFrames(ctx context.Context, m Media) ([][]byte, error) {
	if w.frames.available() {
		return w.frames.sample(ctx, w.lib.path(m), m.Seconds, 3)
	}
	var buf bytes.Buffer
	if err := w.run.refs.firstFrame(ctx, w.lib.path(m), &buf); err != nil {
		return nil, fmt.Errorf("take a frame through ComfyUI (ffmpeg is not installed): %w", err)
	}
	img, err := shrinkBytes(buf.Bytes(), 768)
	if err != nil {
		return nil, err
	}
	return [][]byte{img}, nil
}

// shrink is a library image as a JPEG no larger than px on its long side.
func (w *web) shrink(m Media, px int) ([]byte, error) {
	f, err := w.lib.open(m)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return shrinkBytes(buf.Bytes(), px)
}

func shrinkBytes(data []byte, px int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read the image: %w", err)
	}
	b := src.Bounds()
	scale := min(1, float64(px)/float64(max(b.Dx(), b.Dy())))
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
