package main

import "testing"

func TestControlLooksAreTheControlsNotTheSetting(t *testing.T) {
	pose, _ := controlOf(Media{Origin: originTool, Tool: "pose", Label: "a · Pose"})
	depth, _ := controlOf(Media{Origin: originTool, Tool: "depth", ToolForm: "look=depth&model=frame", Label: "a · Depth sharp"})
	for _, c := range []struct {
		cc   controlClip
		text string
		bad  bool
	}{
		{pose, "a neon-lit street at night", false},
		{pose, "glowing neon lines trace his limbs", true},
		{pose, "follow the pose skeleton of @clip", false},
		{pose, "stick figures dance", true},
		{depth, "against a black background", true},
		{depth, "gray silhouettes move", true},
		{depth, "a man in a gray suit", false},
		{depth, "follow the depth map of @clip", false},
	} {
		if got := c.cc.look.MatchString(c.text); got != c.bad {
			t.Errorf("%s: %q flagged %v, want %v", c.cc.Name, c.text, got, c.bad)
		}
	}
	if cc, ok := controlOf(Media{Origin: originTool, Tool: "depth", Label: "a · Normals stable"}); !ok || cc.Name != "normal map" {
		t.Errorf("an older normals item reads as %+v", cc)
	}
	if _, ok := controlOf(Media{Origin: originUpload}); ok {
		t.Error("an upload reads as a control clip")
	}
	if cc, _ := controlOf(Media{Origin: originTool, Tool: "matte", ToolForm: "look=cutout"}); cc.Kind != "" {
		t.Error("a cutout should not demand a guide word: it is footage of the people")
	}
}
