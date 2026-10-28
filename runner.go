package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// publisher pushes a changed run or media tile to every open page.
type publisher interface {
	run(r Run)
	media(m Media)
}

// errUserCanceled is the cause a run's context carries when the visitor
// cancels it, as opposed to the server stopping.
var errUserCanceled = errors.New("canceled by the visitor")

// runner executes runs and local references in the background. Each runs on
// a context derived from the runner's own, so stopping the server ends the
// waiting, not the provider's work: a run that holds a request id is
// collected again on the next start.
type runner struct {
	st   *store
	lib  *library
	eng  *engines
	refs *localRefs
	pub  publisher
	// cloud reaches Comfy Cloud for images made there; nil in tests.
	cloud *cloudLink
	// frames is ffmpeg, which cuts and joins a long clip's parts; nil in
	// tests.
	frames *frames

	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu      sync.Mutex
	cancels map[string]context.CancelCauseFunc
	// swatchMu keeps two page loads from starting the same swatches.
	swatchMu sync.Mutex
	// local limits how many runs use this machine's GPU at once
	// (Settings, Runs at the same time).
	local *gate
	// succeeded is called with each run that finishes with a result.
	succeeded func(Run, Media)
	// guard runs background work, turning a panic into a crash report.
	guard func(where string, fn func())
	// tools runs the Tools page's jobs; nil in tests that use none.
	tools *toolKit
}

// gate is a counting semaphore whose size can change while runs wait.
type gate struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	used  int
}

func newGate(limit int) *gate {
	g := &gate{limit: max(limit, 1)}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) setLimit(n int) {
	g.mu.Lock()
	g.limit = max(n, 1)
	g.mu.Unlock()
	g.cond.Broadcast()
}

// enter waits for a free slot, or for ctx to end. busy is called once if it
// has to wait.
func (g *gate) enter(ctx context.Context, busy func()) error {
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	waited := false
	for g.used >= g.limit {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !waited && busy != nil {
			waited = true
			g.mu.Unlock()
			busy()
			g.mu.Lock()
			continue
		}
		g.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	g.used++
	return nil
}

func (g *gate) leave() {
	g.mu.Lock()
	g.used--
	g.mu.Unlock()
	g.cond.Broadcast()
}

func newRunner(st *store, lib *library, eng *engines, refs *localRefs, pub publisher) *runner {
	base, stop := context.WithCancel(context.Background())
	return &runner{
		st: st, lib: lib, eng: eng, refs: refs, pub: pub,
		base: base, stop: stop, cancels: map[string]context.CancelCauseFunc{},
		local: newGate(1),
	}
}

// shutdown stops every follow loop and waits for them to save their state.
func (r *runner) shutdown() {
	r.stop()
	r.wg.Wait()
}

// recover picks up what a stopped server left: runs with a request id are
// collected, runs without one never reached the provider and are marked
// interrupted, and references that were generating are marked failed.
func (r *runner) recover(ctx context.Context) error {
	runs, err := r.st.activeRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.RequestID == "" {
			run.Status = runInterrupted
			run.Detail = ""
			run.Error = "The server stopped before this run reached the video provider."
			if r.cloudStyle(run) {
				run.Error += " Cloud style preparation may have been billed."
			} else {
				run.Error += " Nothing was billed."
			}
			run.FinishedAt = now()
			if err := r.st.saveRun(ctx, run); err != nil {
				return err
			}
			continue
		}
		log.Printf("utsushi: collecting run %s (%s %s)", run.ID, run.Engine, run.RequestID)
		r.launch(run, true)
	}
	pending, err := r.st.pendingMedia(ctx)
	if err != nil {
		return err
	}
	for _, m := range pending {
		m.Status = mediaFailed
		m.Error = "The server stopped before this was finished."
		if err := r.st.finishMedia(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// start begins a stored run.
func (r *runner) start(run Run) { r.launch(run, false) }

func (r *runner) launch(run Run, resume bool) {
	ctx, cancel := context.WithCancelCause(r.base)
	r.mu.Lock()
	r.cancels[run.ID] = cancel
	r.mu.Unlock()
	r.wg.Go(func() {
		defer func() {
			r.mu.Lock()
			delete(r.cancels, run.ID)
			r.mu.Unlock()
			cancel(nil)
		}()
		r.safely("run "+run.ID, func() { r.execute(ctx, run, resume) })
	})
}

// cancel stops a run. Once a provider holds it, the provider is asked first;
// if it refuses (the work already started), the run keeps going, because
// abandoning it would not stop the bill, only lose the result.
func (r *runner) cancel(ctx context.Context, id string) error {
	run, err := r.st.run(ctx, id)
	if err != nil {
		return err
	}
	if !runActive(run.Status) {
		return errors.New("the run has already finished")
	}
	if held := heldRequest(run); held != "" {
		en, ok := r.eng.get(run.Engine)
		if !ok {
			return fmt.Errorf("unknown engine %q", run.Engine)
		}
		if err := en.cancel(ctx, held); err != nil {
			return fmt.Errorf("the provider would not cancel it, so it keeps running: %w", err)
		}
	}
	r.mu.Lock()
	stop, ok := r.cancels[id]
	r.mu.Unlock()
	if ok {
		stop(errUserCanceled)
		return nil
	}
	// Not running here (a restart lost the follow loop): record it directly.
	run.Status = runCanceled
	run.Detail = ""
	run.FinishedAt = now()
	if err := r.st.saveRun(ctx, run); err != nil {
		return err
	}
	r.pub.run(run)
	return nil
}

// runTracker records an engine's progress on its run.
type runTracker struct {
	r   *runner
	mu  sync.Mutex
	run *Run
}

func (t *runTracker) queued(requestID string, position *int) {
	t.update(func(run *Run) {
		run.RequestID = requestID
		run.Status = runQueued
		run.Detail = "Queued"
		if position != nil {
			run.Detail = fmt.Sprintf("Queued, position %d", *position+1)
		}
	})
}

func (t *runTracker) running(detail string, progress float64) {
	t.update(func(run *Run) {
		if run.RequestID != "" {
			run.Status = runRunning
		}
		run.Detail = detail
		run.Progress = min(max(progress, 0), 1)
	})
}

// snapshot is a copy of the run. Parts is copied too, so a later update
// never changes a snapshot already handed out.
func (t *runTracker) snapshot() Run {
	t.mu.Lock()
	defer t.mu.Unlock()
	run := *t.run
	run.Parts = slices.Clone(run.Parts)
	return run
}

func (t *runTracker) progress() float64 { return t.snapshot().Progress }

func (t *runTracker) update(change func(*Run)) {
	t.mu.Lock()
	change(t.run)
	snapshot := *t.run
	snapshot.Parts = slices.Clone(snapshot.Parts)
	t.mu.Unlock()
	// The run context may be ending; the state must still land.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.r.st.saveRun(ctx, snapshot); err != nil {
		log.Printf("utsushi: save run %s: %v", snapshot.ID, err)
	}
	t.r.pub.run(snapshot)
}

// collectBackoff spaces the attempts to collect a run after losing contact
// with the provider that holds it: about two and a half minutes in all.
var collectBackoff = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second, 90 * time.Second}

func (r *runner) execute(ctx context.Context, run Run, resume bool) {
	tr := &runTracker{r: r, run: &run}
	en, ok := r.eng.get(run.Engine)
	if !ok {
		r.finish(tr, nil, fmt.Errorf("unknown engine %q", run.Engine))
		return
	}
	err := r.attempt(ctx, en, tr, resume)
	for i, wait := range collectBackoff {
		if err == nil || !retryable(ctx, tr, err) {
			break
		}
		log.Printf("utsushi: run %s lost contact: %v", run.ID, err)
		tr.running(fmt.Sprintf("Lost contact with %s. Trying again (%d of %d)", en.info().Via, i+1, len(collectBackoff)),
			tr.progress())
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if ctx.Err() == nil {
			err = r.attempt(ctx, en, tr, true)
		}
	}
	if err == nil {
		return
	}
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errUserCanceled) {
		// The server is stopping. A run the provider holds is collected on
		// the next start; leave it as it is.
		tr.mu.Lock()
		held := tr.run.RequestID != ""
		tr.mu.Unlock()
		if held {
			return
		}
		tr.update(func(run *Run) {
			run.Status = runInterrupted
			run.Detail = ""
			run.Error = "The server stopped before this run reached the video provider."
			if r.cloudStyle(*run) {
				run.Error += " Cloud style preparation may have been billed."
			} else {
				run.Error += " Nothing was billed."
			}
			run.FinishedAt = now()
		})
		return
	}
	if retryable(ctx, tr, err) {
		tr.update(func(run *Run) {
			run.Status = runLost
			run.Detail = ""
			run.Error = fmt.Sprintf("Lost contact with %s while it held this run (%s). It may still finish: Collect tries again.",
				en.info().Via, describe(err))
			run.FinishedAt = now()
		})
		return
	}
	r.finish(tr, nil, err)
}

// attempt runs or resumes the generation and stores its result. It finishes
// the run on success; on error it leaves the run for the caller to settle.
func (r *runner) attempt(ctx context.Context, en engine, tr *runTracker, resume bool) error {
	run := tr.snapshot()
	// A run on this machine's GPU waits for a free local slot. Billed
	// engines run elsewhere and never wait here.
	if !en.info().Billed && !resume {
		if err := r.local.enter(ctx, func() { tr.running("Waiting for a free slot on this machine", 0) }); err != nil {
			return err
		}
		defer r.local.leave()
	}
	if run.Mode == modeLong {
		return r.attemptShots(ctx, en, tr)
	}
	if len(run.Parts) > 0 {
		return r.attemptParts(ctx, en, tr)
	}
	var out outcome
	var err error
	if resume {
		tr.running("Reconnecting to the provider", run.Progress)
		out, err = en.resume(ctx, run.RequestID, tr)
	} else {
		var t task
		if t, err = r.task(ctx, run); err == nil && run.Mode == modeStyle {
			t, err = r.prepareStyle(ctx, tr, t)
		}
		if err == nil {
			out, err = en.generate(ctx, t, tr)
		}
	}
	if err != nil {
		return err
	}
	tr.running("Saving the result", 1)
	m, err := r.keep(ctx, out, run)
	if err != nil {
		return err
	}
	r.finish(tr, &m, nil)
	r.done(tr.snapshot(), m)
	return nil
}

// retryable reports an error worth collecting again: the provider holds the
// run and has not given a verdict. A failure is the provider's verdict, and
// errOffline is the app missing what it needs; neither improves by waiting.
func retryable(ctx context.Context, tr *runTracker, err error) bool {
	var f failure
	var off errOffline
	return ctx.Err() == nil && tr.snapshot().RequestID != "" && !errors.As(err, &f) && !errors.As(err, &off)
}

func (r *runner) task(ctx context.Context, run Run) (task, error) {
	src, err := r.st.media(ctx, run.SourceID)
	if err != nil {
		return task{}, fmt.Errorf("source video: %w", err)
	}
	refs, err := r.st.mediaByIDs(ctx, run.RefIDs)
	if err != nil {
		return task{}, err
	}
	return task{
		Prompt: run.Prompt, Resolution: run.Resolution, Source: src, Refs: refs,
		Key: run.IdempotencyKey, Seed: seedOf(run),
	}, nil
}

// keep stores a finished generation in the library.
func (r *runner) keep(ctx context.Context, out outcome, run Run) (Media, error) {
	return r.keepAs(ctx, out, originResult, modeLabels[run.Mode]+" result")
}

// keepAs stores a generation in the library under an origin and label.
func (r *runner) keepAs(ctx context.Context, out outcome, origin, label string) (Media, error) {
	var m Media
	var err error
	switch {
	case out.URL != "":
		m, err = r.lib.fetch(ctx, out.URL, label)
		m.Origin = origin
	case out.Write != nil:
		m, err = r.storeStream(ctx, out.Write, origin, label)
	default:
		return Media{}, errors.New("the engine returned no result")
	}
	if err != nil {
		return Media{}, fmt.Errorf("save result: %w", err)
	}
	if err := r.st.addMedia(ctx, m); err != nil {
		return Media{}, errors.Join(err, r.lib.remove(m))
	}
	return m, nil
}

// prepareStyle puts a Style run's style frame at the front of its
// references. A preset uses the selected workflow destination for its first
// frame. The frame is saved on the run, so a retry reuses it.
func (r *runner) prepareStyle(ctx context.Context, tr *runTracker, t task) (task, error) {
	run := tr.snapshot()
	preset, isPreset := presetByKey(run.Style)
	if isPreset {
		t.Look = preset.Look
	}
	if run.StyleImageID == "" {
		if !isPreset {
			return t, errOffline{"This run names no style."}
		}
		refs, err := r.styleRenderer(ctx, run)
		if err != nil {
			return t, err
		}
		label := "Restyling the first frame on this machine"
		if r.cloudStyle(run) {
			label = "Restyling the first frame on Comfy Cloud · billed GPU time"
		}
		tr.running(label, 0)
		var last time.Time
		write := func(ctx context.Context, w io.Writer) error {
			return refs.restyleFrame(ctx, r.lib.path(t.Source), preset.Look, w, func(label string, v float64) {
				if time.Since(last) < 500*time.Millisecond {
					return
				}
				last = time.Now()
				tr.running("Restyling the first frame · "+label, 0)
			})
		}
		frame, err := r.storeStream(ctx, write, originStyle, preset.Name+" frame")
		if err != nil {
			return t, fmt.Errorf("restyle the first frame: %w", err)
		}
		if err := r.st.addMedia(ctx, frame); err != nil {
			return t, errors.Join(err, r.lib.remove(frame))
		}
		tr.update(func(run *Run) { run.StyleImageID = frame.ID })
		run.StyleImageID = frame.ID
	}
	img, err := r.st.media(ctx, run.StyleImageID)
	if err != nil {
		return t, fmt.Errorf("style image: %w", err)
	}
	t.StyleImage = img
	t.Refs = append([]Media{img}, t.Refs...)
	return t, nil
}

// storeStream copies a writer-shaped source into the library.
func (r *runner) storeStream(ctx context.Context, write func(context.Context, io.Writer) error, origin, label string) (Media, error) {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(ctx, pw)) }()
	m, err := r.lib.store(pr, origin, label, 0)
	// Unblock the writer if store stopped early (a refused type, a size cap).
	if cerr := pr.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return m, err
}

func (r *runner) finish(tr *runTracker, result *Media, err error) {
	tr.update(func(run *Run) {
		run.FinishedAt = now()
		run.Detail = ""
		switch {
		case err == nil && result != nil:
			run.Status = runSucceeded
			run.ResultID = result.ID
			run.Progress = 1
			run.Error = ""
			if en, ok := r.eng.get(run.Engine); ok && en.info().Billed {
				run.CostUSD = r.billed(en.info(), *run, result)
			}
		case errors.Is(err, context.Canceled):
			run.Status = runCanceled
		default:
			run.Status = runFailed
			run.Error = describe(err)
			// Before a provider accepts the work, nothing reaches a bill.
			if en, ok := r.eng.get(run.Engine); ok && en.info().Billed {
				if run.RequestID == "" {
					if r.cloudStyle(*run) && run.StyleImageID == "" {
						run.Error += " Cloud style preparation may have been billed."
					} else if r.cloudStyle(*run) {
						run.Error += " The video was not submitted; the cloud style frame was billed separately."
					} else {
						run.Error += " Nothing was billed."
					}
				}
				// Parts made before the failure were billed.
				run.CostUSD = r.billed(en.info(), *run, nil)
			}
		}
	})
}

// billed is a run's estimated cost from published rates: output seconds,
// plus the clip where the model bills it too. A run in parts counts each
// part made, so a run that failed partway still counts what it spent.
func (r *runner) billed(in engineInfo, run Run, result *Media) float64 {
	ctx := context.Background()
	rate, input := in.Rates[run.Resolution], in.InputRates[run.Resolution]
	if len(run.Parts) > 0 {
		var sum float64
		for _, p := range run.Parts {
			if p.ResultID == "" || p.Kept {
				continue
			}
			if m, err := r.st.media(ctx, p.ResultID); err == nil {
				sum += rate*m.Seconds + input*p.Length
			}
		}
		return sum
	}
	if result == nil {
		return 0
	}
	cost := rate * result.Seconds
	if input > 0 {
		if src, err := r.st.media(ctx, run.SourceID); err == nil {
			cost += input * src.Seconds
		}
	}
	return cost
}

// safely runs background work under the crash guard, when one is set.
func (r *runner) safely(where string, fn func()) {
	if r.guard == nil {
		fn()
		return
	}
	r.guard(where, fn)
}

// done runs the success hook after a run is saved as succeeded.
func (r *runner) done(run Run, result Media) {
	if r.succeeded != nil {
		r.succeeded(run, result)
	}
}

// describe is an error as the run card shows it.
func describe(err error) string {
	if err == nil {
		return "The run ended without a result."
	}
	var off errOffline
	if errors.As(err, &off) {
		return off.reason
	}
	var f failure
	if errors.As(err, &f) {
		return clip(sentence(f.msg), 400)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "Could not reach " + hostOf(ue.URL) + " (" + netReason(ue.Err) + ")."
	}
	return clip(sentence(err.Error()), 400)
}

// sentence capitalizes s and ends it with a full stop, so card text reads
// the same whatever produced it.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	if !strings.ContainsRune(".!?)", r[len(r)-1]) {
		r = append(r, '.')
	}
	return string(r)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the server"
	}
	return u.Host
}

// netReason names a transport failure in a few words.
func netReason(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.As(err, &dns):
		return "address not found"
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "timed out"
	}
	return clip(err.Error(), 120)
}

// collect follows a lost run again.
func (r *runner) collect(ctx context.Context, id string) error {
	run, err := r.st.run(ctx, id)
	if err != nil {
		return err
	}
	if run.Status != runLost || run.RequestID == "" {
		return errors.New("only a lost run can be collected again")
	}
	run.Status, run.Error, run.FinishedAt = runRunning, "", time.Time{}
	if err := r.st.saveRun(ctx, run); err != nil {
		return err
	}
	r.pub.run(run)
	r.launch(run, true)
	return nil
}

// ensureSwatches starts a sample tile for each preset that has none, or
// whose last attempt failed. One worker renders them in turn, so they never
// crowd the GPU or the proxy's event streams.
func (r *runner) ensureSwatches(ctx context.Context) error {
	if r.refs.ready(ctx) != nil {
		return nil
	}
	r.swatchMu.Lock()
	defer r.swatchMu.Unlock()
	have, err := r.st.swatches(ctx)
	if err != nil {
		return err
	}
	type job struct {
		m      Media
		prompt string
	}
	var queue []job
	for _, p := range stylePresets {
		if m, ok := have[p.Key]; ok && m.Status != mediaFailed {
			continue
		}
		m := Media{ID: newID(), Kind: kindImage, Origin: originSwatch, Label: p.Key, Status: mediaPending, CreatedAt: now()}
		if err := r.st.addMedia(ctx, m); err != nil {
			return err
		}
		queue = append(queue, job{m, swatchPrompt(p.Look)})
	}
	if len(queue) > 0 {
		r.wg.Go(func() {
			r.safely("swatches", func() {
				for _, j := range queue {
					r.renderInto(j.m, j.prompt, "portrait")
				}
			})
		})
	}
	return nil
}

// renderInto renders a pending image and records it ready or failed,
// pushing its progress to the page.
func (r *runner) renderInto(m Media, prompt, aspect string) {
	pending := m
	var last time.Time
	write := func(ctx context.Context, w io.Writer) error {
		return r.refs.generate(ctx, prompt, aspect, w, func(label string, v float64) {
			// One tile update per half second is plenty for a sampler bar.
			if time.Since(last) < 500*time.Millisecond {
				return
			}
			last = time.Now()
			pending.Note = fmt.Sprintf("%s · %d%%", label, int(v*100))
			r.pub.media(pending)
		})
	}
	stored, err := r.storeStream(r.base, write, m.Origin, m.Label)
	done := m
	if err != nil {
		done.Status = mediaFailed
		done.Error = describe(err)
	} else {
		done.File, done.ContentType, done.Size = stored.File, stored.ContentType, stored.Size
		done.Width, done.Height = stored.Width, stored.Height
		done.Status = mediaReady
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.st.finishMedia(ctx, done); err != nil {
		log.Printf("utsushi: save reference %s: %v", done.ID, err)
	}
	r.pub.media(done)
}
