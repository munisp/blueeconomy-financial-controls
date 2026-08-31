package settlementsync

import (
	"context"
	"errors"
	"log"
	"time"
)

// Run is the supervised sync loop (the outbox-publisher convention): one
// poll per interval until ctx is cancelled. A poll failure is logged and
// retried on the next tick — the cursor + tb_transfer_id uniqueness make
// every retry idempotent; only context cancellation stops the loop.
func Run(ctx context.Context, syncer *Syncer, interval time.Duration, logger *log.Logger) error {
	if syncer == nil {
		return errors.New("settlement syncer is required")
	}
	if interval <= 0 {
		return errors.New("sync interval must be positive")
	}
	if logger == nil {
		logger = log.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		mirrored, err := syncer.SyncOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			logger.Printf("settlement-sync: poll failed (retry next tick): %v", err)
		} else if mirrored > 0 {
			logger.Printf("settlement-sync: mirrored %d collection transfer(s)", mirrored)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
