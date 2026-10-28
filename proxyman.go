package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// proxyAddr is where the managed proxy listens. The proxy's port is fixed by
// design, since SDK clients are pointed at it.
const proxyAddr = "http://127.0.0.1:8189"

// proxyManager runs comfy-api-proxy as a child process, fronting the ComfyUI
// chosen in Settings, and restarts it when that choice changes or the child
// dies. If a proxy someone started is already on the port, it leaves it
// alone and reports it as external: Settings cannot move that one.
type proxyManager struct {
	bin   string
	probe *http.Client
	// addr is where the proxy answers (proxyAddr; a test points it
	// elsewhere), and every how often a proxy found running is checked.
	addr  string
	every time.Duration
	// life ends at shutdown; it bounds the watch on a proxy found running.
	life context.Context
	end  context.CancelFunc
	// takeOver starts Utsushi's own proxy once one found running is gone;
	// a test replaces it.
	takeOver func()

	mu       sync.Mutex
	comfyURL string
	token    string
	cmd      *exec.Cmd
	done     chan struct{} // closed when cmd exits
	external bool
	lastErr  string
	stopping bool
}

// findProxyBinary is UTSUSHI_PROXY_BIN, then comfy-api-proxy on PATH, then
// bin/comfy-api-proxy in the working directory (where task proxy installs it).
func findProxyBinary() (string, error) {
	if p := os.Getenv("UTSUSHI_PROXY_BIN"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("comfy-api-proxy"); err == nil {
		return p, nil
	}
	p, err := filepath.Abs(filepath.Join("bin", "comfy-api-proxy"))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("no comfy-api-proxy binary: set UTSUSHI_PROXY_BIN, put it on PATH, or run task proxy (%w)", err)
	}
	return p, nil
}

func newProxyManager(bin string) *proxyManager {
	life, end := context.WithCancel(context.Background())
	m := &proxyManager{bin: bin, probe: &http.Client{Timeout: 2 * time.Second}, addr: proxyAddr, every: 5 * time.Second,
		life: life, end: end}
	m.takeOver = func() {
		ctx, cancel := context.WithTimeout(m.life, 15*time.Second)
		defer cancel()
		m.mu.Lock()
		comfyURL, token := m.comfyURL, m.token
		m.mu.Unlock()
		if err := m.switchTo(ctx, comfyURL, token); err != nil {
			log.Printf("utsushi: start the proxy after the running one went away: %v", err)
		}
	}
	return m
}

// start brings the proxy up for comfyURL. An external proxy already on the
// port is used as it is.
func (m *proxyManager) start(ctx context.Context, comfyURL, token string) error {
	if m.healthy(ctx) {
		m.mu.Lock()
		m.external = m.cmd == nil
		m.comfyURL, m.token = comfyURL, token
		m.mu.Unlock()
		if m.isExternal() {
			log.Printf("utsushi: a proxy is already running at %s; using it as it is", m.addr)
			// It may be one going away: the last Utsushi's, still stopping
			// after a quick restart. Then nothing would serve the port.
			go m.watchExternal(m.life, m.every)
			return nil
		}
	}
	return m.switchTo(ctx, comfyURL, token)
}

// watchExternal checks a proxy found running and, once it fails two checks
// in a row, takes the port over with Utsushi's own.
func (m *proxyManager) watchExternal(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !m.isExternal() {
			return
		}
		if m.healthy(ctx) {
			misses = 0
			continue
		}
		if misses++; misses < 2 {
			continue
		}
		m.mu.Lock()
		m.external = false
		take := m.takeOver
		m.mu.Unlock()
		log.Printf("utsushi: the proxy at %s went away; starting Utsushi's own", m.addr)
		take()
		return
	}
}

func (m *proxyManager) isExternal() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.external
}

// switchTo restarts the managed proxy fronting comfyURL and waits for it to
// answer its health route.
func (m *proxyManager) switchTo(ctx context.Context, comfyURL, token string) error {
	m.mu.Lock()
	if m.external {
		m.mu.Unlock()
		return errOffline{"A proxy you started yourself is on 127.0.0.1:8189. Stop it so Utsushi can switch servers."}
	}
	m.comfyURL, m.token = comfyURL, token
	m.mu.Unlock()
	if err := m.stopChild(); err != nil {
		return err
	}
	if err := m.spawn(); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("the proxy did not come up in 10 s: %s", m.lastError())
}

// spawn starts the child and a watcher that restarts it if it dies.
func (m *proxyManager) spawn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.Command(m.bin, "run", "--comfyui", m.comfyURL, "--host", "local", "--port", "8189", "--quiet")
	// The token goes through the environment, so it never shows in ps.
	cmd.Env = append(os.Environ(), "COMFY_PROXY_COMFYUI_TOKEN="+m.token)
	// Its own process group keeps a terminal Ctrl-C from reaching it twice,
	// and Pdeathsig stops it if Utsushi dies without running its shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", m.bin, err)
	}
	m.cmd, m.done, m.lastErr = cmd, make(chan struct{}), ""
	done := m.done
	go m.relay(out)
	go func() {
		err := cmd.Wait()
		close(done)
		m.mu.Lock()
		current, stopping := m.cmd == cmd, m.stopping
		if current {
			m.cmd = nil
			if err != nil {
				m.lastErr = err.Error()
			}
		}
		m.mu.Unlock()
		if current && !stopping {
			log.Printf("utsushi: the proxy exited (%v); restarting it", err)
			m.restartLater(1 * time.Second)
		}
	}()
	return nil
}

// restartLater brings a crashed proxy back, waiting longer each time it
// fails again, up to 30 s.
func (m *proxyManager) restartLater(wait time.Duration) {
	time.AfterFunc(wait, func() {
		m.mu.Lock()
		skip := m.stopping || m.cmd != nil
		m.mu.Unlock()
		if skip {
			return
		}
		if err := m.spawn(); err != nil {
			log.Printf("utsushi: restart the proxy: %v", err)
			m.restartLater(min(wait*2, 30*time.Second))
		}
	})
}

// relay copies the child's output into the app's log, keeping the last line
// as the error to show if it fails.
func (m *proxyManager) relay(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		log.Printf("proxy: %s", line)
		m.mu.Lock()
		m.lastErr = line
		m.mu.Unlock()
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Printf("utsushi: read proxy output: %v", err)
	}
}

func (m *proxyManager) lastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// stopChild ends the managed child, politely first.
func (m *proxyManager) stopChild() error {
	m.mu.Lock()
	cmd, done := m.cmd, m.done
	m.cmd = nil
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-done
	return nil
}

// shutdown stops the child for good.
func (m *proxyManager) shutdown() error {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	m.end()
	return m.stopChild()
}

func (m *proxyManager) healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.addr+"/api/v2/health", nil)
	if err != nil {
		return false
	}
	resp, err := m.probe.Do(req)
	if err != nil {
		return false
	}
	if err := resp.Body.Close(); err != nil {
		return false
	}
	return resp.StatusCode == http.StatusOK
}

// status is what Settings shows about the proxy.
func (m *proxyManager) status() (managed bool, comfyURL, lastErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.external, m.comfyURL, m.lastErr
}
