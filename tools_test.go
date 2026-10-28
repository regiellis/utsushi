package main

import (
	"bytes"
	"context"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCannyFindsAStepAndNothingOnAFlatFrame(t *testing.T) {
	const w, h = 16, 8
	flat := bytes.Repeat([]byte{90, 90, 90}, w*h)
	for _, v := range canny(flat, w, h, 100, 200) {
		if v != 0 {
			t.Fatal("a flat frame has edges")
		}
	}
	// Black on the left half, white on the right.
	step := make([]byte, w*h*3)
	for y := range h {
		for x := w / 2; x < w; x++ {
			copy(step[(y*w+x)*3:], []byte{255, 255, 255})
		}
	}
	out := canny(step, w, h, 100, 200)
	for y := range h {
		var cols []int
		for x := range w {
			if out[y*w+x] == 255 {
				cols = append(cols, x)
			}
		}
		// Suppression keeps one column of the two the Sobel lights.
		if len(cols) != 1 || (cols[0] != w/2-1 && cols[0] != w/2) {
			t.Fatalf("row %d edges at %v, want one column at the step", y, cols)
		}
	}
}

func TestCannyFollowsAWeakEdgeOnlyFromAStrongOne(t *testing.T) {
	// A step whose top half is strong (0 to 255) and bottom half weak (0 to
	// 40), with thresholds between: the weak half survives only by touching
	// the strong half.
	const w, h = 12, 12
	img := make([]byte, w*h*3)
	for y := range h {
		v := byte(255)
		if y >= h/2 {
			v = 40
		}
		for x := w / 2; x < w; x++ {
			copy(img[(y*w+x)*3:], []byte{v, v, v})
		}
	}
	rows := func(out []byte) int {
		n := 0
		for y := range h {
			if bytes.Contains(out[y*w:(y+1)*w], []byte{255}) {
				n++
			}
		}
		return n
	}
	if got := rows(canny(img, w, h, 100, 600)); got != h {
		t.Fatalf("weak edge joined to a strong one: %d rows of %d", got, h)
	}
	if got := rows(canny(img, w, h, 200, 600)); got >= h {
		t.Fatalf("an edge under the low threshold still shows on %d rows", got)
	}
}

func TestConformFilterAndFrameTime(t *testing.T) {
	if got := conformFilter("24", "720", "portrait"); got != "crop='min(iw,ih*9/16)':'min(ih,iw*16/9)',"+
		"scale='if(lt(iw,ih),720,-2)':'if(lt(iw,ih),-2,720)':flags=bicubic,scale=trunc(iw/2)*2:trunc(ih/2)*2,fps=24" {
		t.Errorf("conformFilter = %s", got)
	}
	if got := conformFilter("keep", "keep", "square"); got != "crop='min(iw,ih)':'min(iw,ih)',scale=trunc(iw/2)*2:trunc(ih/2)*2" {
		t.Errorf("conformFilter = %s", got)
	}
	src := Media{Seconds: 2}
	for at, want := range map[string]float64{"1.234": 1.23, "-3": 0, "9": 1.95} {
		got, err := frameAt(toolOrder{Source: src, Form: url.Values{"at": {at}}})
		if err != nil || got != want {
			t.Errorf("frameAt(%s) = %v, %v; want %v", at, got, err, want)
		}
	}
}

// testClip puts a 2 s 320x240 30 fps clip with a tone, made by ffmpeg, in
// the library.
func testClip(t *testing.T, w *web) Media {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command(w.frames.bin, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src).CombinedOutput(); err != nil {
		t.Fatalf("make the clip: %v: %s", err, out)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	clip, err := w.lib.store(f, originUpload, "test.mp4", 0)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	clip.Width, clip.Height = 320, 240
	if err := w.st.addMedia(context.Background(), clip); err != nil {
		t.Fatal(err)
	}
	return clip
}

// waitMedia waits up to a minute for an item to stop being pending.
func waitMedia(t *testing.T, w *web, id string) Media {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		m, err := w.st.media(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status != mediaPending {
			return m
		}
	}
	t.Fatalf("%s is still pending after a minute", id)
	return Media{}
}

// TestRetryMakesAFailedItemAgainAsItWasOrdered fails a Grab a frame item
// by hand, presses Retry the way the card does, and gets the frame it
// asked for, in place.
func TestRetryMakesAFailedItemAgainAsItWasOrdered(t *testing.T) {
	w := newToolsWeb(t)
	if !w.frames.available() || w.frames.probe == "" {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	ctx := context.Background()
	clip := testClip(t, w)
	failed := Media{ID: newID(), Kind: kindImage, Origin: originTool, Tool: "frame", SourceID: clip.ID, ToolForm: "at=1",
		Label: "test.mp4 · frame at 1.0 s", Status: mediaFailed, Error: "The server stopped before this was finished.", CreatedAt: now()}
	if err := w.st.addMedia(ctx, failed); err != nil {
		t.Fatal(err)
	}
	post := func(id string) (int, string, string) {
		req := httptest.NewRequest(http.MethodPost, "/tools/items/"+id+"/retry", nil)
		// The handler reads {id} from chi's route context, as routing sets it.
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		c := w.app.NewContext(rec, req)
		if err := w.retryToolItem(c); err != nil {
			t.Fatal(err)
		}
		return rec.Code, rec.Body.String(), rec.Header().Get("HX-Trigger")
	}
	code, body, _ := post(failed.ID)
	if code != http.StatusOK || !strings.Contains(body, `id="tool-`+failed.ID+`"`) || !strings.Contains(body, "hx-sse:connect") {
		t.Fatalf("retry answered %d without the pending card:\n%s", code, body)
	}
	done := waitMedia(t, w, failed.ID)
	if done.Status != mediaReady || done.Kind != kindImage || done.Width != 320 || done.Error != "" || done.File == "" {
		t.Fatalf("retried item = %+v", done)
	}
	// A ready item has nothing to retry, and a lost source cannot be read.
	if _, _, trig := post(failed.ID); !strings.Contains(trig, "Only a failed item") {
		t.Errorf("retrying a ready item: %q", trig)
	}
	orphan := failed
	orphan.ID, orphan.SourceID, orphan.Status = newID(), "gone", mediaFailed
	if err := w.st.addMedia(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, _, trig := post(orphan.ID); !strings.Contains(trig, "source is no longer in the library") {
		t.Errorf("retrying with the source gone: %q", trig)
	}
}

func TestRetryKeepsOnlyItsOwnItem(t *testing.T) {
	w := newToolsWeb(t)
	j := &toolJob{r: w.run, tool: frameTool, outs: []Media{{}, {ID: "x", Label: "kept"}}}
	if err := j.keep(0, "/no/such/file"); err != nil {
		t.Fatalf("a placeholder output was not discarded: %v", err)
	}
}

func TestDepthItemsBeforeSavedOptionsReadThemFromTheLabel(t *testing.T) {
	for label, want := range map[string]string{
		"output.mp4 · Depth stable":       "look=depth&model=video",
		"walk.mp4 · Depth inverted sharp": "look=inverted&model=frame",
		"a · b.mp4 · Normals stable":      "look=normals&model=video",
	} {
		if got := depthTool.formOf(label).Encode(); got != want {
			t.Errorf("%q gives %q, want %q", label, got, want)
		}
	}
}

// TestClipToolsMakeTheirItemsFromARealClip runs Edges, Grab a frame,
// Conform, and Extract audio on a clip ffmpeg makes, through the runner as
// the page starts them, and reads what they made back with ffprobe.
func TestClipToolsMakeTheirItemsFromARealClip(t *testing.T) {
	w := newToolsWeb(t)
	if !w.frames.available() || w.frames.probe == "" {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	ctx := context.Background()
	clip := testClip(t, w)
	orders := []struct {
		tool *tool
		form url.Values
	}{
		{edgesTool, url.Values{"detail": {"medium"}}},
		{frameTool, url.Values{"at": {"1"}}},
		{conformTool, url.Values{"rate": {"24"}, "shape": {"portrait"}}},
		{audioTool, url.Values{}},
	}
	var ids []string
	for _, o := range orders {
		made, err := w.run.makeTool(ctx, o.tool, toolOrder{Source: clip, Form: o.form})
		if err != nil {
			t.Fatalf("%s: %v", o.tool.ID, err)
		}
		ids = append(ids, made[0].ID)
	}
	got := map[string]Media{}
	deadline := time.Now().Add(90 * time.Second)
	for len(got) < len(ids) && time.Now().Before(deadline) {
		for i, id := range ids {
			m, err := w.st.media(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if m.Status != mediaPending {
				got[orders[i].tool.ID] = m
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, o := range orders {
		m, ok := got[o.tool.ID]
		if !ok || m.Status != mediaReady {
			t.Fatalf("%s: %+v", o.tool.ID, m)
		}
		if m.Origin != originTool || m.Tool != o.tool.ID || m.SourceID != clip.ID {
			t.Errorf("%s: origin %s, tool %s, source %s", o.tool.ID, m.Origin, m.Tool, m.SourceID)
		}
	}
	probe := func(m Media, entries string) string {
		out, err := exec.Command(w.frames.probe, "-v", "error", "-count_frames", "-show_entries", entries,
			"-of", "default=nw=1", w.lib.path(m)).Output()
		if err != nil {
			t.Fatalf("ffprobe %s: %v", m.Label, err)
		}
		return string(out)
	}
	if e := got["edges"]; e.Kind != kindVideo || e.Width != 320 || e.Height != 240 ||
		!strings.Contains(probe(e, "stream=nb_read_frames"), "nb_read_frames=60") {
		t.Errorf("edges: %+v %s", e, probe(e, "stream=nb_read_frames"))
	}
	if fr := got["frame"]; fr.Kind != kindImage || fr.Width != 320 || fr.Height != 240 || !strings.HasSuffix(fr.Label, "frame at 1.0 s") {
		t.Errorf("frame: %+v", fr)
	}
	// 320x240 cropped to 9:16 is 135x240, made even: 134x240, at 24 fps.
	if c := got["conform"]; c.Width != 134 || c.Height != 240 || !strings.Contains(probe(c, "stream=nb_read_frames"), "nb_read_frames=48") {
		t.Errorf("conform: %+v %s", c, probe(c, "stream=nb_read_frames"))
	}
	if a := got["audio"]; a.Kind != kindAudio || a.Seconds < 1.9 || a.Seconds > 2.1 {
		t.Errorf("audio: %+v", a)
	}
}
