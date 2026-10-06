package main

import (
	"context"
	"log"
	"time"
)

// runReaper periodically removes expired photos from S3 and the database.
// Expired photos are already hidden by queries; this reclaims the storage.
func (a *App) runReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := a.reapOnce(ctx); err != nil {
			log.Printf("reaper: %v", err)
		} else if n > 0 {
			log.Printf("reaper: removed %d expired photos", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) reapOnce(ctx context.Context) (int, error) {
	if err := a.db.DeleteExpiredSessions(ctx); err != nil {
		return 0, err
	}
	removed := 0
	for {
		photos, err := a.db.ExpiredPhotos(ctx, 100)
		if err != nil || len(photos) == 0 {
			return removed, err
		}
		for _, p := range photos {
			// Keep the row if S3 deletion fails so the next pass retries it.
			if err := a.store.Delete(ctx, p.ObjectKey, p.ThumbKey); err != nil {
				return removed, err
			}
			if err := a.db.DeletePhoto(ctx, p.ID); err != nil {
				return removed, err
			}
			removed++
		}
	}
}
