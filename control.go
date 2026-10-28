package main

import (
	"net/url"
	"regexp"
	"strings"
)

// controlClip is what a control video is, for a clip the Tools page made: a
// depth map, a pose, edges, or a matte. A video model follows one for
// motion and structure, and it holds no people's looks, light, or colors, so
// the language model has to be told what it is and what to say about it.
type controlClip struct {
	// Kind is the word a prompt names the guide by ("depth"), empty for a
	// cutout, which is still footage of the people.
	Kind string
	// Name is how the prompt guidance refers to it: "depth map".
	Name string
	// Shows says what its frames look like; Gives what it carries.
	Shows, Gives string
	// look matches words that describe the control's own appearance, which
	// a video model would draw if the prompt said them.
	look *regexp.Regexp
}

var (
	controlBase  = `gr[ae]yscale|monochrome|black[- ]and[- ]white|(black|dark|plain|empty) (background|backdrop|void)`
	controlDepth = regexp.MustCompile(`(?i)\b(` + controlBase + `|silhouettes?|gr[ae]y (figures?|shapes?|forms?|tones?)|white (figures?|shapes?|forms?)|featureless)\b`)
	controlPose  = regexp.MustCompile(`(?i)\b(` + controlBase + `|stick[- ]figures?|neon (lines|sticks|limbs|figures?)|colou?red (lines|sticks|dots)|glowing (lines|dots))\b`)
	controlEdges = regexp.MustCompile(`(?i)\b(` + controlBase + `|white (out)?lines?|line drawings?|sketch(es|ed)?)\b`)
	controlMatte = regexp.MustCompile(`(?i)\b(` + controlBase + `|silhouettes?|white (figures?|shapes?|forms?))\b`)
	controlCut   = regexp.MustCompile(`(?i)\b(` + controlBase + `)\b`)
)

// controlOf says what control m is, or false for footage. The look comes
// from the order's saved options, else the item's label.
func controlOf(m Media) (controlClip, bool) {
	if m.Origin != originTool {
		return controlClip{}, false
	}
	form, err := url.ParseQuery(m.ToolForm)
	if err != nil {
		form = url.Values{}
	}
	look := form.Get("look")
	has := func(word string) bool { return strings.Contains(m.Label, " · "+word) }
	switch m.Tool {
	case depthTool.ID:
		switch {
		case look == lookNormals || (look == "" && has("Normals")):
			return controlClip{Kind: "depth", Name: "normal map",
				Shows: "a surface-normal map: which way each surface faces, in false colors (blue faces the camera); its colors are not real",
				Gives: "the motion, the people's poses and shapes, the layout of the space, and the camera", look: controlDepth}, true
		case look == lookInverted || (look == "" && has("Depth inverted")):
			return controlClip{Kind: "depth", Name: "depth map",
				Shows: "an inverted depth map: brightness is distance, near is black and far is white; it has no colors, clothes, faces, or light",
				Gives: "the motion, the people's poses and positions, the layout of the space, and the camera", look: controlDepth}, true
		}
		return controlClip{Kind: "depth", Name: "depth map",
			Shows: "a depth map: brightness is distance, near is white and far is black; it has no colors, clothes, faces, or light",
			Gives: "the motion, the people's poses and positions, the layout of the space, and the camera", look: controlDepth}, true
	case poseTool.ID:
		return controlClip{Kind: "pose", Name: "pose",
			Shows: "an OpenPose skeleton: colored sticks and dots on black for each person's body, hands, and face; no bodies, clothes, or setting",
			Gives: "how each person moves and where they stand, and the framing", look: controlPose}, true
	case edgesTool.ID:
		return controlClip{Kind: "edge", Name: "edge map",
			Shows: "Canny edges: white outlines of every shape on black; no colors, light, or textures",
			Gives: "the shapes, the layout, the motion, and the camera", look: controlEdges}, true
	case matteTool.ID:
		if look == "cutout" || (look == "" && has("Cutout")) {
			return controlClip{Name: "cutout",
				Shows: "the people cut out over black; the background is gone",
				Gives: "the people's motion and poses, but no setting", look: controlCut}, true
		}
		return controlClip{Kind: "matte", Name: "matte",
			Shows: "a matte: white where a person is, black everywhere else",
			Gives: "where the people are and how they move", look: controlMatte}, true
	}
	return controlClip{}, false
}

// drives says whether a control video engine can follow it: a depth map,
// a pose, or edges, the controls the MiniMax H3 and LTX-2.3 union models
// take. A normal map, a matte, and a cutout are not among them.
func (cc controlClip) drives() bool {
	return cc.Name == "depth map" || cc.Name == "pose" || cc.Name == "edge map"
}

// guide is the instruction for a control clip the prompt refers to as ref:
// name it as a guide, and give in full what it lacks.
func (cc controlClip) guide(ref string) string {
	s := ref + " is not footage but a " + cc.Name + " control video the video model follows: " + cc.Shows + ". It gives only " + cc.Gives + "."
	if cc.Kind != "" {
		s += " Say in the prompt that the video follows " + ref + " as its " + cc.Name + ", as in \"follow the " + cc.Name + " of " + ref +
			" for the motion and layout\"."
	}
	return s + " Everything it lacks comes from the prompt: each person's whole look from their reference, and the setting with " +
		"real light and colors that suit it. Never describe how the control video itself looks (its grays, black, white, " +
		"outlines, sticks, or silhouettes, or a dark or empty background), because the video model draws whatever the prompt says."
}
