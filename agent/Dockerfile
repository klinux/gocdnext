FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.work ./
COPY server/go.mod server/go.sum* ./server/
COPY agent/go.mod agent/go.sum* ./agent/
COPY cli/go.mod cli/go.sum* ./cli/
COPY plugins/slack/go.mod plugins/slack/go.sum* ./plugins/slack/
RUN cd agent && go mod download
COPY agent ./agent
COPY proto ./proto
RUN cd agent && CGO_ENABLED=0 go build -o /out/gocdnext-agent ./cmd/gocdnext-agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git docker-cli
COPY --from=build /out/gocdnext-agent /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/gocdnext-agent"]
