# gocdnext/rust — Rust build / test / lint runner.
#
# Ships with the official rust image: stable toolchain, cargo,
# rustc, rustup, rustfmt, and clippy. $CARGO_HOME is left as the
# default (/usr/local/cargo) so a caches: mount hitting that path
# Just Works — pairs with the platform's cache entries.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  cargo subcommand + args.
#                                   Examples:
#                                     "test --workspace"
#                                     "clippy --all-targets -- -D warnings"
#                                     "build --release"
#                                     "fmt --check"
#                                     "publish"
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_TOOLCHAIN    (optional)  "stable" (default), "beta",
#                                   "nightly", or a pinned
#                                   "1.83.0". Ensures the matching
#                                   toolchain before running.
#   PLUGIN_RUSTFLAGS    (optional)  exported as RUSTFLAGS for the
#                                   command. Useful for "-D warnings"
#                                   as a global, or "-C target-cpu=..."
#                                   in a release pipeline.
# Pinned to an EXACT version + digest (not the floating 1.83-slim tag) so a
# rebuild is reproducible and a CVE bump is a reviewable one-line change. 1.99.1
# (was 1.83): the stale slim base carried 10 fixable CRITICAL + 900+ HIGH OS
# packages; the version bump + `apt-get upgrade` clears them (Trivy
# --ignore-unfixed: 0 CRITICAL / 0 HIGH). Runs as root (unchanged).
#
# NOTE: this is a large toolchain jump (1.83 → 1.99). clippy and rustfmt gain
# lints and formatting rules across releases, so a pipeline on the default
# `stable` toolchain running `clippy -- -D warnings` or `fmt --check` may surface
# new findings after this bump — pin `PLUGIN_TOOLCHAIN` (e.g. "1.83.0") to hold a
# version. The wrapper inputs are unchanged.
FROM rust:1.99.1-slim@sha256:7eab7ce19273ce785e0ca7578bdf7bc1fcbae621775a8197379a04c9f6e03cd5

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends bash git ca-certificates pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# rustfmt + clippy come pre-installed in the `rust:` image, but
# keep this explicit so a switch to a minimal base still works.
RUN rustup component add rustfmt clippy

COPY entrypoint.sh /usr/local/bin/gocdnext-rust
RUN chmod +x /usr/local/bin/gocdnext-rust

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-rust"]
