# gocdnext/buf — wrap the bufbuild CLI so pipelines call
# `lint`, `breaking`, `generate` etc. without re-pinning the
# image per-job.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  buf subcommand + args, word-split.
#                                   Examples: "lint", "breaking --against .git#branch=main",
#                                   "generate".
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
FROM bufbuild/buf:1.47

# bufbuild/buf is alpine-based; sh is already there but we want
# git for `breaking --against` references and ca-certificates
# for module proxy TLS.
USER root
RUN apk add --no-cache git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-buf
RUN chmod +x /usr/local/bin/gocdnext-buf

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-buf"]
