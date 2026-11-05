# gocdnext/coveralls — upload coverage with Coveralls' official
# Universal Coverage Reporter. The release binary supports the
# formats exposed by plugin.yaml and the parallel report/done flow.
#
# Auth: a repo token (Coveralls cloud) or the per-repo token from
# Coveralls Enterprise. Pipe via secrets:. The entrypoint exports it
# through COVERALLS_REPO_TOKEN; it never puts the token on argv.
FROM alpine:3.22 AS downloader

ARG COVERALLS_VERSION=0.6.22
ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates curl \
    && case "${TARGETARCH}" in \
         amd64) asset=coveralls-linux-x86_64; sha256=34fcaf347627307e505ec207acd617c3bfc6c3e2684b22e760be29eb339c36d5 ;; \
         arm64) asset=coveralls-linux-aarch64; sha256=947e529a54b0c254803947bc3896f8625e51ee81a0d407b2313c8a2513065450 ;; \
         *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL \
       "https://github.com/coverallsapp/coverage-reporter/releases/download/v${COVERALLS_VERSION}/${asset}" \
       -o /coveralls \
    && printf '%s  %s\n' "${sha256}" /coveralls | sha256sum -c - \
    && chmod 0755 /coveralls

FROM alpine:3.22

RUN apk add --no-cache bash ca-certificates git

COPY --from=downloader /coveralls /usr/local/bin/coveralls
COPY entrypoint.sh /usr/local/bin/gocdnext-coveralls
RUN chmod +x /usr/local/bin/gocdnext-coveralls

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-coveralls"]
