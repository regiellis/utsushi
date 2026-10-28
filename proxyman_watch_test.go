package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A proxy found running at start is someone else's until it goes away; then
// Utsushi must take over, or nothing serves the port.
func TestAProxyThatGoesAwayIsTakenOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	m := newProxyManager("unused")
	m.addr, m.every = srv.URL, 20*time.Millisecond
	var tookOver atomic.Int32
	m.takeOver = func() { tookOver.Add(1) }
	t.Cleanup(m.end)
	if err := m.start(context.Background(), "http://comfy:8188", ""); err != nil {
		t.Fatal(err)
	}
	if !m.isExternal() {
		t.Fatal("a proxy already answering should be used as it is")
	}
	time.Sleep(100 * time.Millisecond)
	if tookOver.Load() != 0 {
		t.Fatal("took over a proxy that still answers")
	}
	srv.Close()
	deadline := time.Now().Add(2 * time.Second)
	for tookOver.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tookOver.Load() != 1 {
		t.Fatalf("a proxy that went away was taken over %d times, want once", tookOver.Load())
	}
	if m.isExternal() {
		t.Error("once taken over, the proxy is Utsushi's own")
	}
}
