package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	listenerInitialBackoff = 100 * time.Millisecond
	listenerMaxBackoff     = 5 * time.Second
)

func RunListener(ctx context.Context, pool *pgxpool.Pool, waker *Waker, logger *slog.Logger) {
	backoff := listenerInitialBackoff
	for ctx.Err() == nil {
		connected, err := listen(ctx, pool, waker)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = listenerInitialBackoff
		}
		logger.Warn("capacity listener disconnected", "error", err, "retry_in", backoff)
		if err := waitForRetry(ctx, backoff); err != nil {
			return
		}
		backoff = min(backoff*2, listenerMaxBackoff)
	}
}

func listen(ctx context.Context, pool *pgxpool.Pool, waker *Waker) (connected bool, err error) {
	pooledConnection, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquiring listener connection: %w", err)
	}
	// LISTEN is session state. Hijack the connection so no still-listening
	// connection can return to the pool after an error or cancellation.
	connection := pooledConnection.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if closeErr := connection.Close(closeCtx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing listener connection: %w", closeErr))
		}
	}()

	if _, err := connection.Exec(ctx, "LISTEN capacity_changed"); err != nil {
		return false, fmt.Errorf("subscribing to capacity changes: %w", err)
	}
	for {
		if _, err := connection.WaitForNotification(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return true, nil
			}
			return true, fmt.Errorf("waiting for capacity notification: %w", err)
		}
		waker.Wake()
	}
}
