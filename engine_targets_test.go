package main

import "testing"

func TestWorkflowPickerOffersBothTargets(t *testing.T) {
	where := onCloud
	base := newLTXStyleBase(nil, nil).onCloud(&cloudLink{}, func() string { return where })
	reg := &engines{list: []engine{newRestyler(base, "ltx23-style", 0, nil)}}
	list := reg.forMode(modeStyle)
	if len(list) != 2 {
		t.Fatalf("want local and cloud choices; got %d", len(list))
	}
	for _, target := range []string{onComfyUI, onCloud} {
		id := "ltx23-style@" + target
		en, ok := reg.get(id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		where = onComfyUI
		in := en.info()
		if in.ID != id || in.Billed != (target == onCloud) {
			t.Fatalf("target changed with Settings: %+v", in)
		}
		if in.Limits.MaxRefs != 0 || in.Mode != modeStyle {
			t.Fatal("lost style reference limits")
		}
	}
	if _, ok := reg.get("ltx23-style"); !ok {
		t.Fatal("old run IDs must remain resolvable")
	}
}

func TestOpenImageChoicesKeepTheirTarget(t *testing.T) {
	for _, base := range imageEngines {
		if base.Partner {
			if _, ok := imageEngineByID(base.ID + "@comfyui"); ok {
				t.Fatal("partner model exposed as local")
			}
			continue
		}
		for _, target := range []string{onComfyUI, onCloud} {
			e, ok := imageEngineByID(base.ID + "@" + target)
			if !ok || e.where(onComfyUI) != target || e.where(onCloud) != target {
				t.Fatalf("%s lost explicit %s destination", base.ID, target)
			}
			if e.MaxRefs != base.MaxRefs || e.Family != base.Family {
				t.Fatal("variant lost model behavior")
			}
		}
	}
}

func TestWorkflowChoicesPreserveModelGuidance(t *testing.T) {
	for _, id := range []string{"wan-animate2", "minimax-h3-local", "ltx23-style", "ltx23-control"} {
		for _, target := range []string{onComfyUI, onCloud} {
			if localAdvice(id+"@"+target, hardwareInfo{}).Tier != localAdvice(id, hardwareInfo{}).Tier {
				t.Fatal("variant lost memory guidance")
			}
			if videoRating(modeStyle, id+"@"+target) != videoRating(modeStyle, id) {
				t.Fatal("variant lost task rating")
			}
		}
	}
}
