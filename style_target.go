package main

import "context"

// cloudStyle identifies workflow Style presets whose preparation must use
// the same cloud destination as the video. User-supplied style images need
// no preparation; partner engines retain their existing local preparation.
func (r *runner) cloudStyle(run Run) bool {
	if run.Mode != modeStyle {
		return false
	}
	if _, ok := presetByKey(run.Style); !ok {
		return false
	}
	en, ok := r.eng.get(run.Engine)
	if !ok {
		return false
	}
	style, ok := en.(*restyler)
	if !ok {
		return false
	}
	wf, ok := style.base.(*workflowEngine)
	return ok && wf.inCloud()
}

func (r *runner) styleRenderer(ctx context.Context, run Run) (*localRefs, error) {
	if r.cloudStyle(run) {
		en, _ := r.eng.get(run.Engine)
		wf := en.(*restyler).base.(*workflowEngine)
		client, err := wf.client()
		if err != nil {
			return nil, err
		}
		// Cloud runs the same graph without the local health probe or local
		// model filename fitting.
		return &localRefs{client: client}, nil
	}
	if err := r.refs.ready(ctx); err != nil {
		return nil, errOffline{"Restyling the first frame needs the local proxy. Start it with task run."}
	}
	return r.refs, nil
}
