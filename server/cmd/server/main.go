package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"server/internal/apikey"
	"server/internal/db"
	"server/internal/db/data"
	"server/internal/httpapi"
)

const (
	defaultListenAddress = ":8080"
	authCacheTTL         = 5 * time.Second
	shutdownTimeout      = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout *os.File, logger *slog.Logger) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	queries := data.New(pool)
	switch command {
	case "serve":
		if len(args) != 0 {
			return errors.New("serve does not accept arguments")
		}
		address := os.Getenv("LISTEN_ADDR")
		if address == "" {
			address = defaultListenAddress
		}
		authenticator := httpapi.NewTokenAuthenticator(queries, authCacheTTL)
		controlPlane := httpapi.New(pool, queries, authenticator, logger)
		return serve(ctx, address, controlPlane.Handler, logger)
	case "apikey":
		return apikey.Run(ctx, args, queries, stdout)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func serve(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "address", address)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving http: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving http: %w", err)
		}
		return nil
	}
}
