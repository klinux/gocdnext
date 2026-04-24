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
FROM rust:1.83-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash git ca-certificates pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# rustfmt + clippy come pre-installed in the `rust:` image, but
# keep this explicit so a switch to a minimal base still works.
RUN rustup component add rustfmt clippy

COPY entrypoint.sh /usr/local/bin/gocdnext-rust
RUN chmod +x /usr/local/bin/gocdnext-rust

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-rust"]
