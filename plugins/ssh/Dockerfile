# gocdnext/ssh — primitive deploy-over-SSH plugin. Two modes via
# inputs, combinable: rsync files to a remote host, run a script on
# a remote host, or both. Aimed at the universe the k8s-shaped
# "deployments:" primitive doesn't cover — VPS, bare-metal,
# edge boxes, legacy apps, one-off ops on a DB host.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_HOST         (required)  Remote hostname or IP.
#   PLUGIN_USER         (required)  SSH user.
#   PLUGIN_PORT         (optional)  SSH port. Default 22.
#   PLUGIN_KEY          (one-of)    Private key PEM (multi-line OK).
#                                   Pass via `secrets:` so it's
#                                   masked in logs.
#   PLUGIN_PASSWORD     (one-of)    Password auth (sshpass under
#                                   the hood). Strongly discouraged
#                                   vs. keys but kept for the rare
#                                   legacy host that demands it.
#   PLUGIN_KNOWN_HOSTS  (rec.)      Output of `ssh-keyscan -p $PORT
#                                   $HOST` — one or more lines.
#                                   Required when host_key_check
#                                   is the default (yes).
#   PLUGIN_HOST_KEY     (alt.)      Single host-key line; same shape
#                                   as a known_hosts line. Use when
#                                   you have one fingerprint to
#                                   pin and don't want to manage a
#                                   full known_hosts blob.
#   PLUGIN_HOST_KEY_CHECK (optional) "yes" (default) | "no". Set
#                                   "no" to skip MITM protection —
#                                   logs a warn so the trade-off is
#                                   visible. Never the default.
#   PLUGIN_UPLOAD       (optional)  Path (file or dir) to rsync to
#                                   the remote. Multiple paths
#                                   supported via newline OR comma
#                                   separator.
#   PLUGIN_TARGET       (optional)  Destination dir on the remote.
#                                   Required when PLUGIN_UPLOAD is
#                                   set. Created via rsync's
#                                   `--mkpath` if missing.
#   PLUGIN_RSYNC_OPTS   (optional)  Extra flags passed to rsync.
#                                   Default `-az --delete`.
#   PLUGIN_SCRIPT       (optional)  Multi-line script to run on the
#                                   remote (single ssh invocation).
#                                   Empty = upload-only.
#   PLUGIN_WORKING_DIR  (optional)  cd here on the AGENT before
#                                   resolving PLUGIN_UPLOAD paths.
#                                   Default ".". Has no effect on
#                                   the remote — for that, prefix
#                                   the script with `cd /target`.
FROM alpine:3.20

# openssh-client: ssh + scp + ssh-keyscan
# rsync: efficient uploads with incremental + delete
# sshpass: password auth fallback (kept off the path until needed)
# bash: entrypoint shell — POSIX sh would do but bash arrays make
#       upload-list parsing one-line cleaner.
RUN apk add --no-cache bash openssh-client rsync sshpass

COPY entrypoint.sh /usr/local/bin/gocdnext-ssh
RUN chmod +x /usr/local/bin/gocdnext-ssh

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-ssh"]
