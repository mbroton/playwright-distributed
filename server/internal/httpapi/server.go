package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"server/internal/db/data"
	"server/internal/relay"
	"server/internal/scheduler"
)

const (
	bearerScheme           = "bearer"
	maxConnectMetadataSize = 8 * 1024
)

type Server struct {
	Handler http.Handler
	API     huma.API
}

type serverOptions struct {
	scheduler      *scheduler.Scheduler
	relayManager   *relay.Manager
	defaultBrowser string
	dialTimeout    time.Duration
}

type Option func(*serverOptions)

func WithScheduler(value *scheduler.Scheduler) Option {
	return func(options *serverOptions) {
		options.scheduler = value
	}
}

func WithRelayManager(value *relay.Manager, defaultBrowser string, dialTimeout time.Duration) Option {
	return func(options *serverOptions) {
		options.relayManager = value
		options.defaultBrowser = defaultBrowser
		options.dialTimeout = dialTimeout
	}
}

func New(
	pool *pgxpool.Pool,
	queries *data.Queries,
	authenticator Authenticator,
	logger *slog.Logger,
	options ...Option,
) *Server {
	settings := serverOptions{defaultBrowser: "chromium", dialTimeout: 10 * time.Second}
	for _, option := range options {
		option(&settings)
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("Playwright Distributed Control Plane", "1.0.0")
	config.DocsPath = ""
	config.OpenAPIPath = ""
	config.SchemasPath = ""
	config.CreateHooks = nil
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		bearerScheme: {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "pwd_<random>",
		},
	}
	humaAPI := humago.New(mux, config)

	secured := func(prefix string) *huma.Group {
		group := huma.NewGroup(humaAPI, prefix)
		group.UseModifier(func(operation *huma.Operation, next func(*huma.Operation)) {
			operation.Security = []map[string][]string{{bearerScheme: {}}}
			next(operation)
		})
		group.UseMiddleware(authMiddleware(group, authenticator, logger))
		return group
	}

	registerPublicRoutes(secured("/v1"), queries, settings.scheduler, logger)
	registerWorkerRoutes(secured("/internal"), pool, queries, settings.scheduler, logger)
	registerInfrastructureRoutes(humaAPI, pool, logger)
	if settings.relayManager != nil {
		registerRelayRoutes(
			mux,
			queries,
			authenticator,
			settings.scheduler,
			settings.relayManager,
			settings.defaultBrowser,
			settings.dialTimeout,
			logger,
		)
	}

	return &Server{
		Handler: requestLogger(mux, logger),
		API:     humaAPI,
	}
}

func OpenAPISpec() ([]byte, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(nil, nil, NoAuthAuthenticator{}, logger)
	spec, err := server.API.OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("encoding openapi document: %w", err)
	}
	return spec, nil
}

func registerPublicRoutes(
	api huma.API,
	queries *data.Queries,
	sessionScheduler *scheduler.Scheduler,
	logger *slog.Logger,
) {
	type createSessionInput struct {
		Body struct {
			Browser           string           `json:"browser" enum:"chromium,firefox,webkit"`
			Mode              data.SessionMode `json:"mode,omitempty" default:"default" enum:"default,dedicated"`
			PlaywrightVersion string           `json:"playwright_version,omitempty" pattern:"^[0-9]+\\.[0-9]+(\\.[0-9]+(-[0-9A-Za-z.-]+)?(\\+[0-9A-Za-z.-]+)?)?$"`
			ConnectMetadata   map[string]any   `json:"connect_metadata,omitempty" nullable:"false"`
		}
	}
	type createSessionOutput struct {
		Body Session
	}
	huma.Register(api, huma.Operation{
		OperationID:   "create-session",
		Method:        http.MethodPost,
		Path:          "/sessions",
		Summary:       "Create a session",
		Tags:          []string{"Sessions"},
		DefaultStatus: http.StatusCreated,
		Errors: []int{
			http.StatusTooManyRequests,
			http.StatusUnauthorized,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *createSessionInput) (*createSessionOutput, error) {
		if input.Body.Mode == data.SessionModeDedicated {
			return nil, huma.Error422UnprocessableEntity("dedicated mode is not available yet")
		}
		metadata := input.Body.ConnectMetadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		encodedMetadata, err := json.Marshal(metadata)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("connect_metadata must be a JSON object")
		}
		if len(encodedMetadata) > maxConnectMetadataSize {
			return nil, huma.Error422UnprocessableEntity("connect_metadata must not exceed 8 KiB")
		}
		if sessionScheduler == nil {
			return nil, internalError(logger, "create session", errors.New("scheduler is not configured"))
		}

		versionPrefix := ""
		if input.Body.PlaywrightVersion != "" {
			var ok bool
			versionPrefix, ok = relay.VersionPrefix(input.Body.PlaywrightVersion)
			if !ok {
				return nil, huma.Error422UnprocessableEntity("playwright_version must be semver or major.minor")
			}
		}
		session, err := sessionScheduler.Admit(ctx, scheduler.ClaimRequest{
			Browser:         input.Body.Browser,
			VersionPrefix:   versionPrefix,
			CreatedByKey:    PrincipalFromContext(ctx).KeyID,
			ConnectMetadata: encodedMetadata,
		})
		switch {
		case err == nil:
		case errors.Is(err, scheduler.ErrQueueFull):
			return nil, huma.Error429TooManyRequests("session capacity and queue are full")
		case errors.Is(err, scheduler.ErrWaitTimeout):
			return nil, huma.ErrorWithHeaders(
				huma.Error503ServiceUnavailable("session queue wait timed out"),
				http.Header{"Retry-After": []string{"1"}},
			)
		case errors.Is(err, scheduler.ErrDraining):
			return nil, huma.ErrorWithHeaders(
				huma.Error503ServiceUnavailable("server is shutting down"),
				http.Header{"Retry-After": []string{"1"}},
			)
		case errors.Is(err, context.Canceled):
			return nil, err
		default:
			return nil, internalError(logger, "create session", err)
		}
		response, err := sessionFromData(session)
		if err != nil {
			return nil, internalError(logger, "map created session response", err)
		}
		return &createSessionOutput{Body: response}, nil
	})

	type getSessionInput struct {
		ID uuid.UUID `path:"id" format:"uuid" doc:"Session ID"`
	}
	type getSessionOutput struct {
		Body Session
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-session",
		Method:      http.MethodGet,
		Path:        "/sessions/{id}",
		Summary:     "Get a session",
		Tags:        []string{"Sessions"},
		Errors: []int{
			http.StatusNotFound,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *getSessionInput) (*getSessionOutput, error) {
		session, err := queries.GetSession(ctx, input.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("session not found")
		}
		if err != nil {
			return nil, internalError(logger, "get session", err)
		}
		response, err := sessionFromData(session)
		if err != nil {
			return nil, internalError(logger, "map session response", err)
		}
		return &getSessionOutput{Body: response}, nil
	})

	type deleteSessionOutput struct{}
	huma.Register(api, huma.Operation{
		OperationID:   "delete-session",
		Method:        http.MethodDelete,
		Path:          "/sessions/{id}",
		Summary:       "Terminate a session",
		Description:   "The relay closes within one session heartbeat interval.",
		Tags:          []string{"Sessions"},
		DefaultStatus: http.StatusNoContent,
		Errors: []int{
			http.StatusNotFound,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *getSessionInput) (*deleteSessionOutput, error) {
		_, err := queries.TerminateSession(ctx, input.ID)
		if err == nil {
			return &deleteSessionOutput{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, internalError(logger, "terminate session", err)
		}
		if _, err := queries.GetSession(ctx, input.ID); errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("session not found")
		} else if err != nil {
			return nil, internalError(logger, "get session after termination", err)
		}
		return &deleteSessionOutput{}, nil
	})

	type listWorkersOutput struct {
		Body []Worker `nullable:"false"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "list-workers",
		Method:      http.MethodGet,
		Path:        "/workers",
		Summary:     "List workers",
		Tags:        []string{"Workers"},
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, _ *struct{}) (*listWorkersOutput, error) {
		workers, err := queries.ListWorkers(ctx)
		if err != nil {
			return nil, internalError(logger, "list workers", err)
		}
		response := make([]Worker, 0, len(workers))
		for _, worker := range workers {
			response = append(response, workerFromData(worker))
		}
		return &listWorkersOutput{Body: response}, nil
	})

	type capacityOutput struct {
		Body Capacity
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-capacity",
		Method:      http.MethodGet,
		Path:        "/capacity",
		Summary:     "Get session capacity",
		Tags:        []string{"Sessions"},
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, _ *struct{}) (*capacityOutput, error) {
		if sessionScheduler == nil {
			return nil, internalError(logger, "get capacity", errors.New("scheduler is not configured"))
		}
		capacity, err := sessionScheduler.Capacity(ctx)
		if err != nil {
			return nil, internalError(logger, "get capacity", err)
		}
		response := Capacity{
			Browsers:     make([]BrowserCapacity, 0, len(capacity.Browsers)),
			Queued:       capacity.Queued,
			MaxQueueSize: capacity.MaxQueueSize,
		}
		for _, browser := range capacity.Browsers {
			response.Browsers = append(response.Browsers, BrowserCapacity{
				Browser:        browser.Browser,
				Workers:        browser.Workers,
				MaxSlots:       browser.MaxSlots,
				ActiveSessions: browser.ActiveSessions,
				AvailableSlots: browser.AvailableSlots,
			})
			response.Totals.Workers += browser.Workers
			response.Totals.MaxSlots += browser.MaxSlots
			response.Totals.ActiveSessions += browser.ActiveSessions
			response.Totals.AvailableSlots += browser.AvailableSlots
		}
		return &capacityOutput{Body: response}, nil
	})
}

func registerWorkerRoutes(
	api huma.API,
	pool *pgxpool.Pool,
	queries *data.Queries,
	sessionScheduler *scheduler.Scheduler,
	logger *slog.Logger,
) {
	type registerWorkerInput struct {
		Body struct {
			Address           string `json:"address" format:"uri" pattern:"^wss?://[^\\s/?#]+" maxLength:"512"`
			Browser           string `json:"browser" enum:"chromium,firefox,webkit"`
			PlaywrightVersion string `json:"playwright_version" pattern:"^[0-9]+\\.[0-9]+\\.[0-9]+" minLength:"1" maxLength:"64"`
			MaxSlots          int32  `json:"max_slots" minimum:"1" maximum:"1024"`
		}
	}
	type workerOutput struct {
		Body Worker
	}
	huma.Register(api, huma.Operation{
		OperationID:   "register-worker",
		Method:        http.MethodPost,
		Path:          "/workers",
		Summary:       "Register a worker",
		Tags:          []string{"Workers"},
		DefaultStatus: http.StatusCreated,
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *registerWorkerInput) (*workerOutput, error) {
		// The pattern cannot reject an empty hostname (ws://:3000, ws://user@).
		if parsed, err := url.Parse(input.Body.Address); err != nil || parsed.Hostname() == "" {
			return nil, huma.Error422UnprocessableEntity("address must include a hostname")
		}
		worker, err := registerWorker(ctx, pool, queries, data.RegisterWorkerParams{
			ID:                uuid.New(),
			Address:           input.Body.Address,
			Browser:           input.Body.Browser,
			PlaywrightVersion: input.Body.PlaywrightVersion,
			MaxSlots:          input.Body.MaxSlots,
			Status:            data.WorkerStatusAvailable,
		})
		if err != nil {
			return nil, internalError(logger, "register worker", err)
		}
		return &workerOutput{Body: workerFromData(worker)}, nil
	})

	type heartbeatInput struct {
		ID   uuid.UUID `path:"id" format:"uuid" doc:"Worker ID"`
		Body struct {
			ActiveSessionIDs []uuid.UUID `json:"active_session_ids" nullable:"false" format:"uuid"`
		}
	}
	type heartbeatOutput struct {
		Body struct {
			Status          data.WorkerStatus `json:"status" enum:"available,draining,stalled,shutting_down"`
			Commands        []string          `json:"commands" nullable:"false"`
			StaleSessionIDs []uuid.UUID       `json:"stale_session_ids" nullable:"false" format:"uuid"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "heartbeat-worker",
		Method:      http.MethodPost,
		Path:        "/workers/{id}/heartbeat",
		Summary:     "Heartbeat a worker",
		Tags:        []string{"Workers"},
		Errors: []int{
			http.StatusNotFound,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *heartbeatInput) (*heartbeatOutput, error) {
		var worker data.Worker
		staleIDs := []uuid.UUID{}
		failedIDs := []uuid.UUID{}
		var err error
		if sessionScheduler == nil {
			worker, err = queries.UpdateWorkerHeartbeat(ctx, input.ID)
		} else {
			worker, staleIDs, failedIDs, err = sessionScheduler.Heartbeat(
				ctx,
				input.ID,
				input.Body.ActiveSessionIDs,
			)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("worker not found")
		}
		if err != nil {
			return nil, internalError(logger, "heartbeat worker", err)
		}
		if len(failedIDs) > 0 {
			logger.Warn(
				"worker lost sessions",
				"worker_id", worker.ID,
				"session_ids", failedIDs,
			)
		}
		output := &heartbeatOutput{}
		output.Body.Status = worker.Status
		output.Body.Commands = []string{}
		output.Body.StaleSessionIDs = staleIDs
		return output, nil
	})

	type setStatusInput struct {
		ID   uuid.UUID `path:"id" format:"uuid" doc:"Worker ID"`
		Body struct {
			Status data.WorkerStatus `json:"status" enum:"draining,shutting_down"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "set-worker-status",
		Method:      http.MethodPost,
		Path:        "/workers/{id}/status",
		Summary:     "Request a worker status transition",
		Tags:        []string{"Workers"},
		Errors: []int{
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *setStatusInput) (*workerOutput, error) {
		worker, err := queries.SetWorkerStatus(ctx, data.SetWorkerStatusParams{
			ID:     input.ID,
			Status: input.Body.Status,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, getErr := queries.GetWorker(ctx, input.ID); errors.Is(getErr, pgx.ErrNoRows) {
				return nil, huma.Error404NotFound("worker not found")
			} else if getErr != nil {
				return nil, internalError(logger, "get worker after rejected status transition", getErr)
			}
			return nil, huma.Error409Conflict("invalid worker status transition")
		}
		if err != nil {
			return nil, internalError(logger, "set worker status", err)
		}
		return &workerOutput{Body: workerFromData(worker)}, nil
	})
}

func registerWorker(
	ctx context.Context,
	pool *pgxpool.Pool,
	queries *data.Queries,
	params data.RegisterWorkerParams,
) (_ data.Worker, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return data.Worker{}, fmt.Errorf("beginning worker registration: %w", err)
	}
	defer rollbackTransaction(ctx, tx, &err)
	txQueries := queries.WithTx(tx)

	worker, err := txQueries.RegisterWorker(ctx, params)
	if err != nil {
		return data.Worker{}, fmt.Errorf("inserting worker registration: %w", err)
	}
	if err := txQueries.NotifyCapacityChanged(ctx); err != nil {
		return data.Worker{}, fmt.Errorf("notifying worker capacity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return data.Worker{}, fmt.Errorf("committing worker registration: %w", err)
	}
	return worker, nil
}

func rollbackTransaction(ctx context.Context, tx pgx.Tx, err *error) {
	rollbackErr := tx.Rollback(ctx)
	if rollbackErr == nil || errors.Is(rollbackErr, pgx.ErrTxClosed) {
		return
	}
	*err = errors.Join(*err, fmt.Errorf("rolling back transaction: %w", rollbackErr))
}

func registerInfrastructureRoutes(api huma.API, pool *pgxpool.Pool, logger *slog.Logger) {
	type healthOutput struct {
		Body struct {
			Status string `json:"status"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Check process health",
		Tags:        []string{"Infrastructure"},
	}, func(context.Context, *struct{}) (*healthOutput, error) {
		output := &healthOutput{}
		output.Body.Status = "ok"
		return output, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "readiness",
		Method:      http.MethodGet,
		Path:        "/readyz",
		Summary:     "Check database readiness",
		Tags:        []string{"Infrastructure"},
		Errors:      []int{http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			logger.Warn("database readiness check failed", "error", err)
			return nil, huma.Error503ServiceUnavailable("database unavailable")
		}
		output := &healthOutput{}
		output.Body.Status = "ok"
		return output, nil
	})
}

func internalError(logger *slog.Logger, operation string, err error) error {
	logger.Error(operation, "error", err)
	return huma.Error500InternalServerError("internal server error")
}
