// Command templateapi turns a ComfyUI workflow template (the UI format the
// template browser loads) into the API format a server runs, the way the
// frontend's graphToPrompt does, without a running ComfyUI: node inputs come
// from an object_info dump.
//
//	templateapi TEMPLATE.json OBJECT_INFO.json > NAME.api.json
//
// It flattens subgraphs (inner nodes become "instance:inner"), follows
// reroutes, passes links through bypassed nodes, drops muted and virtual
// ones, and lays each node's widget values onto its inputs in object_info
// order, skipping the seed control and upload values the frontend stores
// beside them.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: templateapi TEMPLATE.json OBJECT_INFO.json > NAME.api.json")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "templateapi:", err)
		os.Exit(1)
	}
}

func run(templatePath, infoPath string, out io.Writer) error {
	var tpl map[string]any
	if err := readJSON(templatePath, &tpl); err != nil {
		return err
	}
	info, err := readObjectInfo(infoPath)
	if err != nil {
		return err
	}
	api, err := convert(tpl, info)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", " ")
	return enc.Encode(api)
}

// readObjectInfo decodes an object_info dump keeping key order: every input
// spec without an input_order (a dynamic combo's options) gets one from the
// order its inputs were written in, which is the widget order.
func readObjectInfo(path string) (map[string]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Read-only: a close error loses nothing.
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "templateapi: close", path, cerr)
		}
	}()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	top, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("object_info is not an object")
	}
	info := map[string]map[string]any{}
	for k, n := range top {
		if m, ok := n.(map[string]any); ok {
			info[k] = m
		}
	}
	return info, nil
}

// decodeOrdered reads one JSON value. Objects with "required" or
// "optional" objects and no "input_order" get one, from their key order.
func decodeOrdered(dec *json.Decoder) (any, error) {
	v, _, err := decodeValue(dec)
	return v, err
}

// decodeValue reads one JSON value and, for an object, its keys in order.
func decodeValue(dec *json.Decoder) (any, []string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	d, isDelim := tok.(json.Delim)
	switch {
	case isDelim && d == '{':
		m := map[string]any{}
		var order []string
		inputs := map[string][]string{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, nil, err
			}
			k, _ := kt.(string)
			v, keys, err := decodeValue(dec)
			if err != nil {
				return nil, nil, err
			}
			m[k] = v
			order = append(order, k)
			if _, isObj := v.(map[string]any); isObj && (k == "required" || k == "optional") {
				inputs[k] = keys
			}
		}
		if _, err := dec.Token(); err != nil {
			return nil, nil, err
		}
		if len(inputs) > 0 && m["input_order"] == nil {
			io := map[string]any{}
			for k, list := range inputs {
				l := make([]any, len(list))
				for i, x := range list {
					l[i] = x
				}
				io[k] = l
			}
			m["input_order"] = io
		}
		return m, order, nil
	case isDelim && d == '[':
		var list []any
		for dec.More() {
			v, _, err := decodeValue(dec)
			if err != nil {
				return nil, nil, err
			}
			list = append(list, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, nil, err
		}
		return list, nil, nil
	}
	if n, ok := tok.(json.Number); ok {
		f, err := n.Float64()
		return f, nil, err
	}
	return tok, nil, nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// link is one edge: from a node's output slot to a node's input slot.
type link struct {
	from     string
	fromSlot int
	to       string
	toSlot   int
	typ      string
}

// scope is one graph level: the top graph or a subgraph's inside.
type scope struct {
	prefix string // "" at the top, "129:" inside instance 129
	nodes  map[string]map[string]any
	links  map[string]link // by link id
	sub    *subgraphFrame  // nil at the top
}

// subgraphFrame is what an instance hands its inside: the outer scope and
// the instance node, so subgraph inputs can be traced outward.
type subgraphFrame struct {
	outer    *scope
	instance map[string]any
	def      map[string]any
}

// source is where an input's value comes from: a node output, or a value.
type source struct {
	node  string
	slot  int
	value any
	isVal bool
}

type converter struct {
	info map[string]map[string]any
	defs map[string]map[string]any // subgraph definitions by id
	api  map[string]any
}

func convert(tpl map[string]any, info map[string]map[string]any) (map[string]any, error) {
	c := &converter{info: info, defs: map[string]map[string]any{}, api: map[string]any{}}
	if defs, ok := tpl["definitions"].(map[string]any); ok {
		subs, _ := defs["subgraphs"].([]any)
		for _, s := range subs {
			if sg, ok := s.(map[string]any); ok {
				c.defs[str(sg["id"])] = sg
			}
		}
	}
	top := newScope("", tpl, nil)
	return c.api, c.emit(top)
}

func newScope(prefix string, g map[string]any, sub *subgraphFrame) *scope {
	s := &scope{prefix: prefix, nodes: map[string]map[string]any{}, links: map[string]link{}, sub: sub}
	nodes, _ := g["nodes"].([]any)
	for _, n := range nodes {
		if m, ok := n.(map[string]any); ok {
			s.nodes[str(m["id"])] = m
		}
	}
	links, _ := g["links"].([]any)
	for _, l := range links {
		switch v := l.(type) {
		case []any: // [id, from, from_slot, to, to_slot, type]
			if len(v) >= 6 {
				s.links[str(v[0])] = link{str(v[1]), num(v[2]), str(v[3]), num(v[4]), str(v[5])}
			}
		case map[string]any:
			s.links[str(v["id"])] = link{str(v["origin_id"]), num(v["origin_slot"]), str(v["target_id"]), num(v["target_slot"]), str(v["type"])}
		}
	}
	return s
}

// emit writes every real node of a scope, descending into subgraphs.
func (c *converter) emit(s *scope) error {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := s.nodes[id]
		typ := str(n["type"])
		if def, ok := c.defs[typ]; ok {
			if num(n["mode"]) == 2 || num(n["mode"]) == 4 {
				continue
			}
			inner := newScope(s.prefix+id+":", def, &subgraphFrame{outer: s, instance: n, def: def})
			if err := c.emit(inner); err != nil {
				return err
			}
			continue
		}
		if virtual(typ) || num(n["mode"]) == 2 || num(n["mode"]) == 4 {
			continue
		}
		spec, ok := c.info[typ]
		if !ok {
			return fmt.Errorf("node %s%s: %s is not in object_info", s.prefix, id, typ)
		}
		inputs, err := c.inputs(s, n, spec)
		if err != nil {
			return fmt.Errorf("node %s%s (%s): %w", s.prefix, id, typ, err)
		}
		entry := map[string]any{"class_type": typ, "inputs": inputs}
		if t := str(n["title"]); t != "" {
			entry["_meta"] = map[string]any{"title": t}
		}
		c.api[s.prefix+id] = entry
	}
	return nil
}

func virtual(typ string) bool {
	switch typ {
	case "Note", "MarkdownNote", "Reroute", "PrimitiveNode", "Label (rgthree)":
		return true
	}
	return false
}

// inputs lays the node's widget values and links onto its object_info
// inputs.
func (c *converter) inputs(s *scope, n map[string]any, spec map[string]any) (map[string]any, error) {
	out := map[string]any{}
	links := map[string]string{} // input name -> link id
	ins, _ := n["inputs"].([]any)
	for _, raw := range ins {
		in, ok := raw.(map[string]any)
		if !ok || in["link"] == nil {
			continue
		}
		links[str(in["name"])] = str(in["link"])
	}
	widgets, _ := n["widgets_values"].([]any)
	if m, ok := n["widgets_values"].(map[string]any); ok {
		// Some custom nodes store widgets by name.
		maps.Copy(out, m)
	}
	w := &widgetReader{values: widgets}
	saved := map[string]bool{}
	for _, raw := range ins {
		if in, ok := raw.(map[string]any); ok && in["widget"] != nil {
			saved[str(in["name"])] = true
		}
	}
	inputSpec, _ := spec["input"].(map[string]any)
	order := inputOrder(spec)
	for _, section := range []string{"required", "optional"} {
		sec, _ := inputSpec[section].(map[string]any)
		for _, name := range order[section] {
			if err := c.fillAs(s, out, links, w, "", name, sec[name], saved); err != nil {
				return nil, err
			}
		}
	}
	// Links to inputs object_info lists under another parent (autogrow
	// slots such as "model.images.image_1") are kept as they are named.
	for name, id := range links {
		if _, done := out[name]; done {
			continue
		}
		src, err := c.resolve(s, id)
		if err != nil {
			return nil, err
		}
		if src != nil {
			out[name] = src.value
			if !src.isVal {
				out[name] = []any{src.node, src.slot}
			}
		}
	}
	return out, nil
}

// inputOrder is object_info's input_order when given, else map order
// sorted, which object_info dumps from ComfyUI always give.
func inputOrder(spec map[string]any) map[string][]string {
	order := map[string][]string{}
	if io, ok := spec["input_order"].(map[string]any); ok {
		for k, v := range io {
			list, _ := v.([]any)
			for _, x := range list {
				order[k] = append(order[k], str(x))
			}
		}
		return order
	}
	inputSpec, _ := spec["input"].(map[string]any)
	for _, section := range []string{"required", "optional"} {
		sec, _ := inputSpec[section].(map[string]any)
		for k := range sec {
			order[section] = append(order[section], k)
		}
		sort.Strings(order[section])
	}
	return order
}

// widgetReader hands out a node's widget values in order.
type widgetReader struct {
	values []any
	i      int
}

func (w *widgetReader) next() (any, bool) {
	if w.i >= len(w.values) {
		return nil, false
	}
	v := w.values[w.i]
	w.i++
	return v, true
}

// skipIf drops the next value when it is one of the given strings: the
// seed control ("randomize") and upload button ("image") values.
func (w *widgetReader) skipIf(words ...string) {
	if w.i < len(w.values) {
		if s, ok := w.values[w.i].(string); ok {
			for _, word := range words {
				if s == word {
					w.i++
					return
				}
			}
		}
	}
}

var widgetTypes = map[string]bool{"INT": true, "FLOAT": true, "STRING": true, "BOOLEAN": true, "COMBO": true}

// isWidgetType is a widget type, or a multi-type input ("FLOAT,INT") with
// a widget type among its types.
func isWidgetType(typ string) bool {
	for t := range strings.SplitSeq(typ, ",") {
		if widgetTypes[strings.TrimSpace(t)] {
			return true
		}
	}
	return false
}

// fillAs sets one input: a link when there is one, else the next widget
// value, recursing into a dynamic combo's chosen option. saved names the
// inputs the template's node marks as widgets.
func (c *converter) fillAs(s *scope, out map[string]any, links map[string]string, w *widgetReader, parent, name string, raw any, saved map[string]bool) error {
	full := name
	if parent != "" {
		full = parent + "." + name
	}
	def, _ := raw.([]any)
	if len(def) == 0 {
		return nil
	}
	opts := map[string]any{}
	if len(def) > 1 {
		opts, _ = def[1].(map[string]any)
	}
	typ, isName := def[0].(string)
	isWidget := !isName || isWidgetType(typ) || typ == "COMFY_DYNAMICCOMBO_V3" || opts["widgetType"] != nil
	if opts["forceInput"] == true {
		isWidget = false
	}
	// The saved node is the better witness: an input it marks as a widget
	// held a value when the template was saved.
	if saved[full] {
		isWidget = true
	}
	var value any
	var hasValue bool
	if isWidget {
		value, hasValue = w.next()
		if typ == "INT" && opts["control_after_generate"] != nil || name == "seed" || name == "noise_seed" {
			w.skipIf("fixed", "increment", "decrement", "randomize")
		}
		if slices.ContainsFunc([]string{"image_upload", "video_upload", "audio_upload", "upload"}, func(up string) bool { return opts[up] != nil }) {
			w.skipIf("image", "video", "audio", "upload")
		}
	}
	if id, ok := links[full]; ok {
		src, err := c.resolve(s, id)
		if err != nil {
			return err
		}
		if src != nil {
			if src.isVal {
				out[full] = src.value
			} else {
				out[full] = []any{src.node, src.slot}
			}
			if typ != "COMFY_DYNAMICCOMBO_V3" {
				return nil
			}
		}
	}
	if !hasValue {
		// The template predates this input (a node gained it later): the
		// server needs it, so it takes its default, as the frontend would.
		if def, ok := opts["default"]; ok && isWidget {
			if _, set := out[full]; !set {
				out[full] = def
			}
		}
		return nil
	}
	if _, set := out[full]; !set {
		out[full] = value
	}
	if typ == "COMFY_DYNAMICCOMBO_V3" {
		options, _ := opts["options"].([]any)
		for _, o := range options {
			om, _ := o.(map[string]any)
			if str(om["key"]) != str(out[full]) {
				continue
			}
			sub, _ := om["inputs"].(map[string]any)
			for _, section := range []string{"required", "optional"} {
				sec, _ := sub[section].(map[string]any)
				names := make([]string, 0, len(sec))
				for k := range sec {
					names = append(names, k)
				}
				if order, ok := sub["input_order"].(map[string]any); ok {
					if list, ok := order[section].([]any); ok {
						names = names[:0]
						for _, x := range list {
							names = append(names, str(x))
						}
					}
				} else {
					sort.Strings(names)
				}
				for _, k := range names {
					if err := c.fillAs(s, out, links, w, full, k, sec[k], saved); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

var errLoop = errors.New("link loop")

// resolve follows a link to the real node output or value behind it.
func (c *converter) resolve(s *scope, linkID string) (*source, error) {
	return c.resolveDepth(s, linkID, 0)
}

func (c *converter) resolveDepth(s *scope, linkID string, depth int) (*source, error) {
	if depth > 64 {
		return nil, errLoop
	}
	l, ok := s.links[linkID]
	if !ok {
		return nil, nil
	}
	if l.from == "-10" { // a subgraph input: trace it on the instance
		return c.subgraphInput(s, l.fromSlot, depth)
	}
	n, ok := s.nodes[l.from]
	if !ok {
		return nil, fmt.Errorf("link %s comes from missing node %s", linkID, l.from)
	}
	typ := str(n["type"])
	switch {
	case typ == "Reroute":
		return c.firstInput(s, n, depth)
	case typ == "PrimitiveNode":
		ws, _ := n["widgets_values"].([]any)
		if len(ws) == 0 {
			return nil, nil
		}
		return &source{value: ws[0], isVal: true}, nil
	case num(n["mode"]) == 2:
		return nil, nil
	case num(n["mode"]) == 4:
		return c.bypass(s, n, l, depth)
	}
	if def, ok := c.defs[typ]; ok { // a subgraph instance's output
		inner := newScope(s.prefix+l.from+":", def, &subgraphFrame{outer: s, instance: n, def: def})
		outs, _ := def["outputs"].([]any)
		if l.fromSlot >= len(outs) {
			return nil, fmt.Errorf("subgraph %s has no output %d", typ, l.fromSlot)
		}
		o, _ := outs[l.fromSlot].(map[string]any)
		ids, _ := o["linkIds"].([]any)
		if len(ids) == 0 {
			return nil, nil
		}
		return c.resolveDepth(inner, str(ids[0]), depth+1)
	}
	return &source{node: s.prefix + l.from, slot: l.fromSlot}, nil
}

// subgraphInput traces input slot of the current subgraph to the instance:
// its outer link, or its promoted widget value.
func (c *converter) subgraphInput(s *scope, slot, depth int) (*source, error) {
	if s.sub == nil {
		return nil, nil
	}
	defIns, _ := s.sub.def["inputs"].([]any)
	if slot >= len(defIns) {
		return nil, fmt.Errorf("subgraph has no input %d", slot)
	}
	want := str(defIns[slot].(map[string]any)["name"])
	ins, _ := s.sub.instance["inputs"].([]any)
	for _, raw := range ins {
		in, _ := raw.(map[string]any)
		if str(in["name"]) == want && in["link"] != nil {
			return c.resolveDepth(s.sub.outer, str(in["link"]), depth+1)
		}
	}
	// A promoted widget: the instance's widget values are its subgraph's
	// widget-type inputs, in order.
	values, _ := s.sub.instance["widgets_values"].([]any)
	i := 0
	for _, raw := range defIns {
		d, _ := raw.(map[string]any)
		if !widgetTypes[str(d["type"])] {
			continue
		}
		if i >= len(values) {
			return nil, nil
		}
		v := values[i]
		i++
		if str(d["name"]) == want {
			return &source{value: v, isVal: true}, nil
		}
		// A seed's control value follows it.
		if str(d["type"]) == "INT" && i < len(values) {
			if sv, ok := values[i].(string); ok && (sv == "randomize" || sv == "fixed" || sv == "increment" || sv == "decrement") {
				i++
			}
		}
	}
	return nil, nil
}

func (c *converter) firstInput(s *scope, n map[string]any, depth int) (*source, error) {
	ins, _ := n["inputs"].([]any)
	for _, raw := range ins {
		in, _ := raw.(map[string]any)
		if in["link"] != nil {
			return c.resolveDepth(s, str(in["link"]), depth+1)
		}
	}
	return nil, nil
}

// bypass passes a link through a bypassed node: from the input of the same
// type as the output it was taken from, as the frontend does.
func (c *converter) bypass(s *scope, n map[string]any, l link, depth int) (*source, error) {
	outs, _ := n["outputs"].([]any)
	outType := l.typ
	if l.fromSlot < len(outs) {
		if o, ok := outs[l.fromSlot].(map[string]any); ok {
			outType = str(o["type"])
		}
	}
	ins, _ := n["inputs"].([]any)
	for _, raw := range ins {
		in, _ := raw.(map[string]any)
		if in["link"] != nil && str(in["type"]) == outType {
			return c.resolveDepth(s, str(in["link"]), depth+1)
		}
	}
	return nil, nil
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func num(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}
