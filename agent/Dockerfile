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
RUN cd agent && go mod download
COPY agent ./agent
RUN cd agent && CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /out/gocdnext-agent ./cmd/gocdnext-agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git docker-cli
COPY --from=build /out/gocdnext-agent /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/gocdnext-agent"]
