package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// keepUpload reuses an identical upload, including uploads made before this
// feature existed. Matching size and kind narrow the files we need to hash.
// Results keep a separate library copy so deleting their run remains safe.
func (w *web) keepUpload(ctx context.Context, fresh Media) (kept Media, err error) {
	w.uploadMu.Lock()
	defer w.uploadMu.Unlock()
	kept = fresh
	defer func() {
		if err != nil || kept.ID != fresh.ID {
			err = errors.Join(err, w.lib.remove(fresh))
		}
	}()
	candidates, err := w.st.uploadCandidates(ctx, fresh)
	if err != nil {
		return Media{}, err
	}
	if len(candidates) > 0 {
		digest, err := w.lib.digest(fresh)
		if err != nil {
			return Media{}, err
		}
		for _, old := range candidates {
			if err := ctx.Err(); err != nil {
				return Media{}, err
			}
			oldDigest, err := w.lib.digest(old)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Media{}, err
			}
			if oldDigest == digest {
				return old, nil
			}
		}
	}
	if err := w.st.addMedia(ctx, fresh); err != nil {
		return Media{}, err
	}
	return fresh, nil
}

func (s *store) uploadCandidates(ctx context.Context, m Media) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+mediaColumns+` FROM media
  WHERE origin = ? AND kind = ? AND size = ? AND status = ?
  ORDER BY created_at, id`, originUpload, m.Kind, m.Size, mediaReady)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMedia)
}

func (l *library) digest(m Media) (sum [sha256.Size]byte, err error) {
	f, err := l.open(m)
	if err != nil {
		return sum, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
