package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestReferencePicksSurviveAReload saves a pick the way the studio script
// does and reads it back as the page would, in pick order, without the
// image deleted since.
func TestReferencePicksSurviveAReload(t *testing.T) {
	w := newToolsWeb(t)
	ctx := context.Background()
	var ids []string
	for _, label := range []string{"a.png", "b.png", "c.png"} {
		m := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: label, Label: label, Status: mediaReady, CreatedAt: now()}
		if err := w.st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	save := func(mode string, refs ...string) int {
		form := url.Values{"mode": {mode}, "refs": refs}
		req := httptest.NewRequest(http.MethodPost, "/studio/refs", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		err := w.rememberRefsHandler(w.app.NewContext(rec, req))
		if err != nil {
			return http.StatusBadRequest
		}
		return rec.Code
	}
	// Picked third, first, then one since deleted.
	if code := save(modeMotion, ids[2], ids[0], "gone"); code >= 300 {
		t.Fatalf("saving answered %d", code)
	}
	got, err := w.rememberedRefs(ctx, modeMotion)
	if err != nil {
		t.Fatal(err)
	}
	if want := `["` + ids[2] + `","` + ids[0] + `"]`; got != want {
		t.Errorf("motion opens with %s, want %s", got, want)
	}
	if got, err := w.rememberedRefs(ctx, modeLong); err != nil || got != "[]" {
		t.Errorf("long video opens with %s, %v; want nothing picked", got, err)
	}
	if code := save("nope", ids[0]); code < 400 {
		t.Errorf("an unknown mode was saved (%d)", code)
	}
}
