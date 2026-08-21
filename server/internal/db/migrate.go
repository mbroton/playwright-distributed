package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"server/internal/db/migrations"
)

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("creating migration locker: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		sqlDB,
		migrations.Files,
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("creating migration provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("applying database migrations: %w", err)
	}

	return nil
}
