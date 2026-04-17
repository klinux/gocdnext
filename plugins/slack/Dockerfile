FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/slack-plugin .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/slack-plugin /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/slack-plugin"]
