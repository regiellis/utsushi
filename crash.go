package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// crashes records panics. A report holds the panic's type, a clipped
// message, the stack, and versions: never prompts, media, or keys, which
// live in request bodies and the database, not on the stack. Reports are
// always written to the data directory; with sending on in Settings and an
// endpoint set, each is also posted there.
type crashes struct {
	dir   string
	prefs *prefsStore
	post  *http.Client
}

type crashReport struct {
	Time    time.Time `json:"time"`
	Where   string    `json:"where"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	Stack   string    `json:"stack"`
	Version string    `json:"version"`
	Go      string    `json:"go"`
	OS      string    `json:"os"`
}

func newCrashes(dir string, prefs *prefsStore) (*crashes, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &crashes{dir: dir, prefs: prefs, post: &http.Client{Timeout: 10 * time.Second}}, nil
}

// record writes a report for a recovered panic value.
func (c *crashes) record(where string, v any) {
	msg := clip(strings.Join(strings.Fields(fmt.Sprint(v)), " "), 200)
	rep := crashReport{
		Time: time.Now().UTC(), Where: where, Kind: fmt.Sprintf("%T", v), Message: msg,
		Stack: string(debug.Stack()), Version: buildVersion(), Go: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH,
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		log.Printf("utsushi: encode crash report: %v", err)
		return
	}
	name := filepath.Join(c.dir, rep.Time.Format("20060102-150405.000")+".json")
	if err := os.WriteFile(name, data, 0o640); err != nil {
		log.Printf("utsushi: write crash report: %v", err)
	}
	log.Printf("utsushi: recovered panic in %s: %s (report %s)", where, msg, name)
	if c.prefs == nil {
		return
	}
	if p := c.prefs.get().About; p.CrashReports && p.CrashEndpoint != "" {
		go c.send(p.CrashEndpoint, data)
	}
}

func (c *crashes) send(endpoint string, data []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		log.Printf("utsushi: send crash report: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.post.Do(req)
	if err != nil {
		log.Printf("utsushi: send crash report: %v", err)
		return
	}
	if err := resp.Body.Close(); err != nil {
		log.Printf("utsushi: send crash report: %v", err)
	}
	if resp.StatusCode >= 300 {
		log.Printf("utsushi: send crash report: %s answered %s", endpoint, resp.Status)
	}
}

// count is how many reports are on disk.
func (c *crashes) count() int {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// middleware turns a handler panic into a report and a 500.
func (c *crashes) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				c.record(r.Method+" "+r.URL.Path, v)
				http.Error(w, "Something broke on the server. A crash report was saved.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// guard runs fn, recording a panic instead of crashing the app.
func (c *crashes) guard(where string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			c.record(where, v)
		}
	}()
	fn()
}

// buildVersion is the app's VCS revision, from the Go build info.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "development build"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += " (modified)"
	}
	return rev
}

// sdkVersion is the version of the Go SDK this app was built with.
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == "github.com/regiellis/comfy-api-proxy-go" {
			if d.Replace != nil {
				return "this checkout"
			}
			return d.Version
		}
	}
	return "unknown"
}
