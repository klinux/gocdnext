.DEFAULT_GOAL := help

BIN_DIR := bin
GO := go

# Pull local dev config in so targets like migrate-up + admin-* see
# GOCDNEXT_DATABASE_URL without the operator having to export it by
# hand. `-include` = no error if the file is missing (fresh clones,
# CI with pure env). `export` forwards every var to recipe shells.
-include .env
export

## help: print this help
help:
	@awk '/^## / {sub(/^## */, ""); sub(/: /, ":\t", $$0); printf "  %s\n", $$0}' $(MAKEFILE_LIST)

## env-setup: copy .env.example to .env (first-time onboarding)
env-setup:
	@if [ -f .env ]; then \
		echo ".env already exists — not overwriting"; \
	else \
		cp .env.example .env && echo "created .env from .env.example — edit secrets before \`make dev\`"; \
	fi

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

## plugins: build all plugin images locally (gocdnext/<name>:latest). Pipelines
## that reference these images via `uses:` pick them up without needing a
## registry push while we're still in the internal-first rollout phase.
plugins:
	@for dir in plugins/*/; do \
		name=$$(basename $$dir); \
		if [ -f $$dir/Dockerfile ]; then \
			echo "==> building gocdnext/$$name"; \
			docker build -t gocdnext/$$name:latest $$dir; \
		fi; \
	done

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

## admin-create-user: seed or rotate a local user; usage: make admin-create-user EMAIL=you@org.com [ROLE=admin]
admin-create-user: build
	@if [ -z "$(EMAIL)" ]; then \
		echo "usage: make admin-create-user EMAIL=you@org.com [ROLE=admin|user|viewer]"; exit 1; \
	fi
	./$(BIN_DIR)/gocdnext admin create-user --email "$(EMAIL)" --role "$(or $(ROLE),admin)"

## admin-reset-password: rotate a local user's password; usage: make admin-reset-password EMAIL=you@org.com
admin-reset-password: build
	@if [ -z "$(EMAIL)" ]; then \
		echo "usage: make admin-reset-password EMAIL=you@org.com"; exit 1; \
	fi
	./$(BIN_DIR)/gocdnext admin reset-password --email "$(EMAIL)"

.PHONY: help env-setup build test lint proto plugins dev stop db-up db-down migrate-up migrate-status admin-create-user admin-reset-password
