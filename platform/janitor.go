package platform

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RunRetention deletes events older than keep, once at the start and then daily, until ctx ends.
// keep of 0 keeps them forever and the function returns at once.
func (s *Store) RunRetention(ctx context.Context, keep time.Duration, log *slog.Logger) {
	if keep <= 0 {
		return
	}
	purge := func() {
		n, err := s.PurgeEvents(ctx, time.Now().Add(-keep))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("Retention - Store", "detail", "could not purge old events: "+err.Error())
		case err == nil && n > 0:
			log.Info("Retention - Store", "detail", fmt.Sprintf("deleted %d events older than %d days", n, int(keep.Hours()/24)))
		}
	}
	purge()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}
