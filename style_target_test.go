package main

import (
	"context"
	"testing"
)

func TestCloudStylePreparationDoesNotRequireLocalComfyUI(t *testing.T) {
	cloud, err := newCloudLink("http://127.0.0.1:1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	local, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	base := newLTXStyleBase(local, nil).onCloud(cloud, func() string { return onComfyUI })
	r := &runner{refs: local, eng: &engines{list: []engine{newRestyler(base, "ltx23-style", 0, nil)}}}
	run := Run{Engine: "ltx23-style@cloud", Mode: modeStyle, Style: "chunky-low-poly"}
	refs, err := r.styleRenderer(context.Background(), run)
	if err != nil {
		t.Fatalf("cloud preparation required unavailable local server: %v", err)
	}
	if refs.client != cloud.cloud() || refs.models != nil {
		t.Fatal("cloud preparation used local client or model index")
	}
	run.Engine = "ltx23-style@comfyui"
	if _, err := r.styleRenderer(context.Background(), run); err == nil {
		t.Fatal("local preparation ignored unavailable proxy")
	}
}
