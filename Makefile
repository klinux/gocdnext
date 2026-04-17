.DEFAULT_GOAL := help

BIN_DIR := bin
GO := go

## help: print this help
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

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

## up: start docker-compose dev stack
up:
	docker compose up -d postgres minio
	@echo "postgres ready on :5432, minio on :9000 (console :9001)"

## down: stop docker-compose dev stack
down:
	docker compose down

## migrate-up: apply database migrations
migrate-up:
	cd server && goose -dir migrations postgres "$${GOCDNEXT_DATABASE_URL}" up

## migrate-status: show migration status
migrate-status:
	cd server && goose -dir migrations postgres "$${GOCDNEXT_DATABASE_URL}" status

.PHONY: help build test lint proto up down migrate-up migrate-status
