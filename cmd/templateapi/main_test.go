package main

import (
	"encoding/json"
	"testing"
)

const objectInfo = `{
 "LoadImage": {"input": {"required": {"image": [["a.png"], {"image_upload": true}]}}, "output": ["IMAGE"]},
 "KSampler": {"input": {"required": {"model": ["MODEL"], "seed": ["INT", {"control_after_generate": true}], "steps": ["INT"], "latent_image": ["LATENT"]}}, "output": ["LATENT"]},
 "Blur": {"input": {"required": {"image": ["IMAGE"], "radius": ["INT"], "passes": ["INT", {"default": 2}]}}, "output": ["IMAGE"]},
 "Save": {"input": {"required": {"images": ["IMAGE"], "prefix": ["STRING"]}}, "output": []},
 "Rate": {"input": {"required": {"frames": ["INT"], "rate": ["FLOAT,INT", {"widgetType": "FLOAT"}], "batch": ["INT"]}}, "output": ["LATENT"]},
 "Pick": {"input": {"required": {"model": ["COMFY_DYNAMICCOMBO_V3", {"options": [
   {"key": "B", "inputs": {"required": {"zeta": ["INT"], "alpha": ["STRING"]}}}]}]}}, "output": ["IMAGE"]}
}`

// template: LoadImage(1) -> bypassed Blur(2) -> Reroute(3) -> subgraph
// instance(4) whose inside is Blur(1) taking a promoted widget, -> Save(5);
// a KSampler(6) with a seed control value; a dynamic combo node (7).
const template = `{
 "nodes": [
  {"id": 1, "type": "LoadImage", "widgets_values": ["cat.png", "image"], "outputs": [{"type": "IMAGE"}]},
  {"id": 2, "type": "Blur", "mode": 4, "inputs": [{"name": "image", "type": "IMAGE", "link": 10}], "outputs": [{"type": "IMAGE"}], "widgets_values": [3]},
  {"id": 3, "type": "Reroute", "inputs": [{"name": "", "type": "*", "link": 11}]},
  {"id": 4, "type": "sg-1", "inputs": [{"name": "img", "type": "IMAGE", "link": 12}], "widgets_values": [7]},
  {"id": 5, "type": "Save", "inputs": [{"name": "images", "type": "IMAGE", "link": 13}], "widgets_values": ["out"]},
  {"id": 6, "type": "KSampler", "widgets_values": [42, "randomize", 8]},
  {"id": 7, "type": "Pick", "widgets_values": ["B", 5, "x"]},
  {"id": 8, "type": "MarkdownNote", "widgets_values": ["notes"]},
  {"id": 9, "type": "Rate", "widgets_values": [97, 25, 1]}
 ],
 "links": [[10, 1, 0, 2, 0, "IMAGE"], [11, 2, 0, 3, 0, "IMAGE"], [12, 3, 0, 4, 0, "IMAGE"], [13, 4, 0, 5, 0, "IMAGE"]],
 "definitions": {"subgraphs": [{
  "id": "sg-1", "name": "Soften",
  "inputs": [{"name": "img", "type": "IMAGE"}, {"name": "radius", "type": "INT"}],
  "outputs": [{"name": "out", "type": "IMAGE", "linkIds": [3]}],
  "nodes": [{"id": 1, "type": "Blur", "inputs": [{"name": "image", "type": "IMAGE", "link": 1}, {"name": "radius", "type": "INT", "link": 2, "widget": {"name": "radius"}}], "widgets_values": [1]}],
  "links": [
   {"id": 1, "origin_id": -10, "origin_slot": 0, "target_id": 1, "target_slot": 0, "type": "IMAGE"},
   {"id": 2, "origin_id": -10, "origin_slot": 1, "target_id": 1, "target_slot": 1, "type": "INT"},
   {"id": 3, "origin_id": 1, "origin_slot": 0, "target_id": -20, "target_slot": 0, "type": "IMAGE"}]
 }]}
}`

func TestConvertFlattensAndFollowsLikeTheFrontend(t *testing.T) {
	var tpl map[string]any
	if err := json.Unmarshal([]byte(template), &tpl); err != nil {
		t.Fatal(err)
	}
	var info map[string]map[string]any
	if err := json.Unmarshal([]byte(objectInfo), &info); err != nil {
		t.Fatal(err)
	}
	// input_order the way readObjectInfo adds it from key order. Blur's
	// "passes" came after the template was saved, so it takes its default.
	info["Blur"]["input_order"] = map[string]any{"required": []any{"image", "radius", "passes"}}
	info["Rate"]["input_order"] = map[string]any{"required": []any{"frames", "rate", "batch"}}
	info["KSampler"]["input_order"] = map[string]any{"required": []any{"model", "seed", "steps", "latent_image"}}
	info["Pick"]["input"].(map[string]any)["required"].(map[string]any)["model"].([]any)[1].(map[string]any)["options"].([]any)[0].(map[string]any)["inputs"].(map[string]any)["input_order"] =
		map[string]any{"required": []any{"zeta", "alpha"}}
	api, err := convert(tpl, info)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(api)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"1":{"class_type":"LoadImage","inputs":{"image":"cat.png"}},` +
		`"4:1":{"class_type":"Blur","inputs":{"image":["1",0],"passes":2,"radius":7}},` +
		`"5":{"class_type":"Save","inputs":{"images":["4:1",0],"prefix":"out"}},` +
		`"6":{"class_type":"KSampler","inputs":{"seed":42,"steps":8}},` +
		`"7":{"class_type":"Pick","inputs":{"model":"B","model.alpha":"x","model.zeta":5}},` +
		`"9":{"class_type":"Rate","inputs":{"batch":1,"frames":97,"rate":25}}}`
	if string(got) != want {
		t.Errorf("converted\n%s\nwant\n%s", got, want)
	}
}
