package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"server/internal/db/data"
)

const bearerScheme = "bearer"

type Server struct {
	Handler http.Handler
	API     huma.API
}

func New(
	pool *pgxpool.Pool,
	queries *data.Queries,
	authenticator Authenticator,
	logger *slog.Logger,
) *Server {
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

	registerPublicRoutes(secured("/v1"), queries, logger)
	registerWorkerRoutes(secured("/internal"), queries, logger)
	registerInfrastructureRoutes(humaAPI, pool, logger)

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

func registerPublicRoutes(api huma.API, queries *data.Queries, logger *slog.Logger) {
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
}

func registerWorkerRoutes(api huma.API, queries *data.Queries, logger *slog.Logger) {
	type registerWorkerInput struct {
		Body struct {
			Address           string `json:"address" format:"uri" pattern:"^wss?://"`
			Browser           string `json:"browser" enum:"chromium,firefox,webkit"`
			PlaywrightVersion string `json:"playwright_version" minLength:"1"`
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
			http.StatusServiceUnavailable,
		},
	}, func(ctx context.Context, input *registerWorkerInput) (*workerOutput, error) {
		worker, err := queries.RegisterWorker(ctx, data.RegisterWorkerParams{
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
		ID uuid.UUID `path:"id" format:"uuid" doc:"Worker ID"`
	}
	type heartbeatOutput struct {
		Body struct {
			Status   data.WorkerStatus `json:"status" enum:"available,draining,stalled,shutting_down"`
			Commands []string          `json:"commands" nullable:"false"`
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
		worker, err := queries.UpdateWorkerHeartbeat(ctx, input.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("worker not found")
		}
		if err != nil {
			return nil, internalError(logger, "heartbeat worker", err)
		}
		output := &heartbeatOutput{}
		output.Body.Status = worker.Status
		output.Body.Commands = []string{}
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
