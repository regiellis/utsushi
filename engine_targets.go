package main

import "strings"

// Explicit target IDs keep a new run's execution destination stable across
// Settings changes and restarts. Unsuffixed IDs still resolve old run history.
func engineModelID(id string) string {
	base, target, ok := strings.Cut(id, "@")
	if ok && (target == onComfyUI || target == onCloud) {
		return base
	}
	return id
}

func engineAt(en engine, target string) (engine, bool) {
	if target != onComfyUI && target != onCloud {
		return nil, false
	}
	switch e := en.(type) {
	case *workflowEngine:
		clone := *e
		clone.about.ID = e.about.ID + "@" + target
		clone.where = func() string { return target }
		return &clone, true
	case *restyler:
		base, ok := engineAt(e.base, target)
		if !ok {
			return nil, false
		}
		clone := *e
		clone.base, clone.id = base, e.id+"@"+target
		return &clone, true
	default:
		return nil, false
	}
}

func (e *engines) choices() []engine {
	var out []engine
	for _, en := range e.list {
		local, ok := engineAt(en, onComfyUI)
		if !ok {
			out = append(out, en)
			continue
		}
		cloud, _ := engineAt(en, onCloud)
		out = append(out, local, cloud)
	}
	return out
}

func (e *engines) preferredID(id, target string) string {
	if engineModelID(id) != id {
		return id
	}
	if en, ok := e.get(id); ok {
		if choice, ok := engineAt(en, target); ok {
			return choice.info().ID
		}
	}
	return id
}
