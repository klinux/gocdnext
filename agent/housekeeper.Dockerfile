# Housekeeper sidecar image (isolated mode). The agent execs into this
# container to tar + compress cache/artifact content out of the job pod, and
# the identical image backs the `cache-fetch` init container that restores
# caches in. Busybox already provides sh + tar + gzip (production has long run
# `tar -czf … -T` here); this image adds `zstd` so the cache store can flip to
# zstd -T0 (#274) and the restore can decompress zstd blobs. Detection is by
# magic bytes in the agent, so this image reads both gzip and zstd caches.
#
# Deliberately tiny and dependency-free (no repo build): fast pull for a
# sidecar that idles between execs.
FROM alpine:3.20

# zstd pulls in libzstd; pin the apk-provided version implicitly to the 3.20
# repo snapshot. `--no-cache` keeps the layer lean.
RUN apk add --no-cache zstd

LABEL org.opencontainers.image.title="gocdnext-housekeeper" \
      org.opencontainers.image.description="Isolated-mode cache/artifact tar+compress sidecar (gzip+zstd)."

# No ENTRYPOINT: the engine sets the container command explicitly
# (sleep loop for housekeeper; marker-wait for cache-fetch).
