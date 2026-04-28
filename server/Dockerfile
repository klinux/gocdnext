FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.work go.work.sum* ./
COPY server/go.mod server/go.sum* ./server/
COPY agent/go.mod agent/go.sum* ./agent/
COPY cli/go.mod cli/go.sum* ./cli/
COPY proto/go.mod proto/go.sum* ./proto/
COPY plugins/slack/go.mod plugins/slack/go.sum* ./plugins/slack/
COPY proto ./proto
RUN cd server && go mod download
COPY server ./server
RUN cd server && CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /out/gocdnext-server ./cmd/gocdnext-server

# Bake the official plugin catalogue into a clean tree of just the
# plugin.yaml manifests — Dockerfiles, entrypoints, and slack's Go
# sources are stripped so the runtime image stays slim. The catalog
# loader walks <root>/<name>/plugin.yaml, so the layout in /catalog
# matches what's at <repo>/plugins/<name>/plugin.yaml on disk.
COPY plugins ./plugins
RUN mkdir -p /catalog && \
    find ./plugins -mindepth 2 -maxdepth 2 -name plugin.yaml -print | \
    while read -r f; do \
        name=$(basename "$(dirname "$f")"); \
        mkdir -p "/catalog/$name" && cp "$f" "/catalog/$name/plugin.yaml"; \
    done

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/gocdnext-server /usr/local/bin/
COPY --from=build /catalog /etc/gocdnext/plugins
ENV GOCDNEXT_PLUGIN_CATALOG_DIR=/etc/gocdnext/plugins
EXPOSE 8153 8154
ENTRYPOINT ["/usr/local/bin/gocdnext-server"]
