package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxConnections    = 20
	minConnections    = 2
	maxConnectionIdle = 30 * time.Minute
	maxConnectionAge  = time.Hour
	healthCheckPeriod = time.Minute
)

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing database dsn: %w", err)
	}

	config.MaxConns = maxConnections
	config.MinConns = minConnections
	config.MaxConnIdleTime = maxConnectionIdle
	config.MaxConnLifetime = maxConnectionAge
	config.HealthCheckPeriod = healthCheckPeriod

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
