SQLC_VERSION := v1.31.1

.PHONY: generate openapi
generate:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

openapi:
	go run ./cmd/openapi > openapi.yaml
