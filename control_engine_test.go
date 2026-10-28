package main

import (
	"strings"
	"testing"
)

// TestControlEnginesFollowTheClipInsteadOfAPreprocessor loads the two
// control engines from their real workflow exports: each control input
// reads the clip's own frames, and the preprocessor the template ran
// (SDPose and its RT-DETR detector, MoGe) is gone with its model loaders.
func TestControlEnginesFollowTheClipInsteadOfAPreprocessor(t *testing.T) {
	for _, c := range []struct {
		engine *workflowEngine
		gone   []string // node classes that must not remain
	}{
		{newMiniMaxH3ControlEngine(nil, nil), []string{"SDPoseKeypointExtractor", "SDPoseDrawKeypoints", "RTDETR_detect", "PreviewImage", "CheckpointLoaderSimple"}},
		{newLTXControlEngine(nil, nil), []string{"MoGeInference", "MoGeRender", "LoadMoGeModel"}},
	} {
		wf, b, err := c.engine.load()
		if err != nil {
			t.Fatalf("%s: %v", c.engine.about.ID, err)
		}
		for id, raw := range wf.Graph {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, class := range c.gone {
				if node["class_type"] == class {
					t.Errorf("%s still has %s (%s)", c.engine.about.ID, class, id)
				}
			}
		}
		for _, in := range b.Control {
			node, input, _ := strings.Cut(in, ".")
			inputs, ok := wf.Graph[node].(map[string]any)["inputs"].(map[string]any)
			if !ok {
				t.Fatalf("%s: no inputs on %s", c.engine.about.ID, node)
			}
			link, ok := inputs[input].([]any)
			if !ok || len(link) != 2 || link[0] != b.ControlFrom {
				t.Errorf("%s: %s reads %v, want the clip's frames from %s", c.engine.about.ID, in, inputs[input], b.ControlFrom)
			}
		}
		// The output still reaches the clip loader and the model.
		if _, ok := wf.Graph[b.Output]; !ok {
			t.Errorf("%s: the output node was pruned", c.engine.about.ID)
		}
		node, _, _ := strings.Cut(b.Video, ".")
		if _, ok := wf.Graph[node]; !ok {
			t.Errorf("%s: the clip loader was pruned", c.engine.about.ID)
		}
	}
	// The Pose engine shares the template and keeps its SDPose.
	wf, _, err := newMiniMaxH3LocalEngine(nil, nil).load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range wf.Graph {
		if node, ok := raw.(map[string]any); ok && node["class_type"] == "SDPoseKeypointExtractor" {
			found = true
		}
	}
	if !found {
		t.Error("the Pose engine lost its SDPose")
	}
}

func TestControlEnginesRefuseAClipThatIsNoControl(t *testing.T) {
	l := newMiniMaxH3ControlEngine(nil, nil).about.Limits
	ref := []Media{{ID: "r", Kind: kindImage}}
	footage := task{Source: Media{Kind: kindVideo, Origin: originUpload, Seconds: 5}, Refs: ref, Resolution: "480p"}
	if f := validate(l, footage); len(f["source"]) == 0 {
		t.Error("footage was taken as a control clip")
	}
	normals := task{Source: Media{Kind: kindVideo, Origin: originTool, Tool: "depth", ToolForm: "look=normals", Seconds: 5}, Refs: ref, Resolution: "480p"}
	if f := validate(l, normals); len(f["source"]) == 0 {
		t.Error("a normal map was taken as a control clip")
	}
	pose := task{Source: Media{Kind: kindVideo, Origin: originTool, Tool: "pose", Label: "a · Pose", Seconds: 5}, Refs: ref, Resolution: "480p"}
	if f := validate(l, pose); len(f["source"]) != 0 {
		t.Errorf("a pose clip was refused: %v", f["source"])
	}
}
