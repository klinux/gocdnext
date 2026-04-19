.DEFAULT_GOAL := help

BIN_DIR := bin
GO := go

## help: print this help
help:
	@awk '/^## / {sub(/^## */, ""); sub(/: /, ":\t", $$0); printf "  %s\n", $$0}' $(MAKEFILE_LIST)

## build: build server, agent and cli
build:
	@mkdir -p $(BIN_DIR)
	cd server && $(GO) build -o ../$(BIN_DIR)/gocdnext-server ./cmd/gocdnext-server
	cd agent  && $(GO) build -o ../$(BIN_DIR)/gocdnext-agent  ./cmd/gocdnext-agent
	cd cli    && $(GO) build -o ../$(BIN_DIR)/gocdnext        ./cmd/gocdnext

## test: run all go tests with the race detector
test:
	cd server && $(GO) test -race ./...
	cd agent  && $(GO) test -race ./...
	cd cli    && $(GO) test -race ./...

## lint: run golangci-lint
lint:
	golangci-lint run ./server/... ./agent/... ./cli/...

## proto: regenerate protobuf stubs (requires protoc + buf)
proto:
	cd proto && buf generate

## dev: boot the full local stack (postgres + server + agent + web) with hot reload
dev:
	@bash scripts/dev.sh

## stop: tear down `make dev`
stop:
	@bash scripts/stop.sh

## db-up: start ONLY postgres (when running server/agent/web outside)
db-up:
	docker compose up -d postgres
	@echo "postgres ready on :5432"

## db-down: stop postgres container
db-down:
	docker compose stop postgres

## migrate-up: apply database migrations
migrate-up:
	cd server && goose -dir migrations postgres "$${GOCDNEXT_DATABASE_URL}" up

## migrate-status: show migration status
migrate-status:
	cd server && goose -dir migrations postgres "$${GOCDNEXT_DATABASE_URL}" status

.PHONY: help build test lint proto dev stop db-up db-down migrate-up migrate-status
