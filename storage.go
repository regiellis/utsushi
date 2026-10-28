package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// nameTokens are the placeholders a result file name may use.
const nameTokens = "{date} {time} {mode} {engine} {style} {v} {id}"

// exportName fills a file-name pattern for a finished run. Unknown text
// stays as written; path separators are replaced, so a pattern cannot leave
// the output folder.
func exportName(pattern string, r Run, version string, at time.Time) string {
	if strings.TrimSpace(pattern) == "" {
		pattern = "{date}_{engine}_{v}"
	}
	if version == "" {
		version = "v1"
	}
	style := r.Style
	if style == "" && r.Mode == modeStyle {
		style = "image"
	}
	repl := strings.NewReplacer(
		"{date}", at.Local().Format("2006-01-02"),
		"{time}", at.Local().Format("150405"),
		"{mode}", r.Mode,
		"{engine}", r.Engine,
		"{style}", style,
		"{v}", version,
		"{id}", r.ID[:min(8, len(r.ID))],
	)
	name := repl.Replace(pattern)
	name = strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c < 0x20 {
			return '-'
		}
		return c
	}, name)
	return strings.Trim(strings.ReplaceAll(name, "__", "_"), "_-. ")
}

// exportResult copies a finished result into the output folder from
// Settings, under the file-name pattern. A name already taken gets a
// counter. Failures are logged: the result is safe in the library either way.
func (w *web) exportResult(r Run, result Media) {
	p := w.prefs.get().Storage
	if strings.TrimSpace(p.OutDir) == "" {
		return
	}
	dir, err := expandHome(p.OutDir)
	if err != nil {
		log.Printf("utsushi: export run %s: %v", r.ID, err)
		return
	}
	version := ""
	if v := w.viewRun(context.Background(), r); v.Version != "" {
		version, _, _ = strings.Cut(v.Version, "/")
	}
	name := exportName(p.NamePattern, r, version, r.FinishedAt)
	if err := copyResult(w.lib, result, dir, name); err != nil {
		log.Printf("utsushi: export run %s: %v", r.ID, err)
	}
}

func copyResult(lib *library, m Media, dir, name string) (err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	ext := filepath.Ext(m.File)
	src, err := lib.open(m)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	for i := 1; i < 1000; i++ {
		target := filepath.Join(dir, name+ext)
		if i > 1 {
			target = filepath.Join(dir, fmt.Sprintf("%s-%d%s", name, i, ext))
		}
		dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			return errors.Join(err, dst.Close(), os.Remove(target))
		}
		return dst.Close()
	}
	return fmt.Errorf("no free name for %s in %s", name, dir)
}

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}

// cleanup deletes finished runs older than Settings allow, hourly, with the
// files only they own. Active and lost runs are never touched.
func (w *web) cleanup(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if err := w.cleanOnce(ctx); err != nil {
			log.Printf("utsushi: storage cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *web) cleanOnce(ctx context.Context) error {
	days := w.prefs.get().Storage.CleanDays
	if days <= 0 {
		return nil
	}
	old, err := w.st.finishedBefore(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		return err
	}
	for _, r := range old {
		if err := w.removeRun(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// removeRun deletes a run with the files only it owns: its result, its
// restyled frame, their previews, and, when Settings keep no copies of
// sources, its source clip once no other run uses it.
func (w *web) removeRun(ctx context.Context, id string) error {
	r, owned, err := w.st.deleteRun(ctx, id)
	if err != nil {
		return err
	}
	for _, m := range owned {
		if err := w.lib.remove(m); err != nil {
			log.Printf("utsushi: remove media %s of run %s: %v", m.ID, r.ID, err)
		}
		if err := w.frames.forget(m.ID); err != nil {
			log.Printf("utsushi: forget previews of %s: %v", m.ID, err)
		}
	}
	if w.prefs.get().Storage.KeepSources || r.SourceID == "" {
		return nil
	}
	used, err := w.st.sourceInUse(ctx, r.SourceID)
	if err != nil || used {
		return err
	}
	m, err := w.st.deleteMedia(ctx, r.SourceID)
	if errors.Is(err, errNotFound) || errors.Is(err, errInUse) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := w.lib.remove(m); err != nil {
		log.Printf("utsushi: remove source %s: %v", m.ID, err)
	}
	return w.frames.forget(m.ID)
}

// dirSize totals a directory's files, for the Storage tab.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// humanBytes is a size such as "3.2 GB".
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// checkWritable makes the output folder if needed and proves a file can be
// written there.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cannot make the output folder %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".utsushi-write-test-*")
	if err != nil {
		return fmt.Errorf("cannot write to the output folder %s: %w", dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	return os.Remove(name)
}
