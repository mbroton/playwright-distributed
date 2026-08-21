package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing database dsn: %w", err)
	}
	if config.MaxConns < 2 {
		// Keep one pooled connection as operational headroom. The hijacked
		// listener connection is outside pool accounting after acquisition.
		return nil, errors.New("database pool max_conns must be at least 2 for operational headroom")
	}

	if config.MaxConnLifetimeJitter == 0 {
		// Zero jitter makes connections created at boot expire together.
		// This can cause reconnect storms across replicas.
		config.MaxConnLifetimeJitter = 5 * time.Minute
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("creating database pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return pool, nil
}
