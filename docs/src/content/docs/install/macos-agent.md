---
title: macOS agent
description: Install a native shell agent on a Mac for Xcode and iOS builds.
---

iOS builds require Xcode, so they must run on a macOS host. gocdnext does not
try to put Xcode in a container: the native agent uses the existing `shell`
engine and executes a pipeline's `script:` directly on the Mac. Jobs select it
with agent tags such as `macos` and `arm64`.

The release publishes native agents for Apple Silicon (`darwin/arm64`) and
Intel (`darwin/amd64`). The installer selects the host architecture, verifies
the release's keyless Cosign signature and checksum, and installs a
`launchd` system service that runs as an unprivileged macOS account.

## Before you install

You need:

- a supported Mac with a non-root account dedicated to builds;
- `cosign` (`brew install cosign`);
- private network reachability from the Mac to the server's gRPC port (8154);
- an agent registration token.

The installer verifies the release with `cosign verify-blob`, which checks
Sigstore's **Rekor transparency log**. The Mac therefore needs outbound HTTPS to
the public Sigstore/Rekor endpoints **during installation**. An air-gapped Mac
cannot run the installer as-is — prepare it separately (mirror the release
assets and verify against an offline Rekor bundle, or verify the signed
`checksums.txt` on a connected host and copy the vetted binary over).

The agent connection currently uses plaintext gRPC. Keep port 8154 on a
private network or VPN; do **not** expose it directly to the internet. The HTTP
ingress used for the dashboard is not a substitute for this private gRPC
route.

For the simplest registration path, enable agent auto-registration on the
server and use the same high-entropy token on the Mac:

```yaml title="values.yaml"
agent:
  autoRegister: true
  tokenSecret:
    existingSecret: gocdnext-agent-token # key: token
```

This is a shared fleet credential: any machine that has it can become a
worker. Deployments that need a distinct credential per agent should leave
auto-registration off and pre-provision the Mac's exact name and token through
their operator/database workflow.

## Install the service

Download the installer **and** the signed checksums, then authenticate the
installer **before** running it. The script Cosign-verifies the binary it
installs, but that self-check cannot vouch for the script itself — verify its
hash here first, or you would be executing unauthenticated code (that may call
`sudo`):

```bash
export GOCDNEXT_VERSION=v0.131.0 # replace with the release you deploy
base="https://github.com/klinux/gocdnext/releases/download/${GOCDNEXT_VERSION}"

curl --proto '=https' --tlsv1.2 -fsSLO "${base}/gocdnext-agent-install-macos.sh"
curl --proto '=https' --tlsv1.2 -fsSLO "${base}/checksums.txt"
curl --proto '=https' --tlsv1.2 -fsSLO "${base}/checksums.txt.sig"
curl --proto '=https' --tlsv1.2 -fsSLO "${base}/checksums.txt.pem"

# 1. Confirm checksums.txt was signed by this repo's release workflow at this tag.
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity "https://github.com/klinux/gocdnext/.github/workflows/release.yml@refs/tags/${GOCDNEXT_VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt

# 2. Confirm the installer's hash against the now-trusted checksums, THEN run it.
grep ' gocdnext-agent-install-macos.sh$' checksums.txt | shasum -a 256 -c -
chmod +x gocdnext-agent-install-macos.sh
```

Supply the non-secret settings, then read the token without putting it in
shell history or process arguments:

```bash
export GOCDNEXT_SERVER_ADDR=ci-grpc.internal.example.com:8154
export GOCDNEXT_AGENT_NAME=mac-mini-01
export GOCDNEXT_AGENT_CAPACITY=1

read -r -s -p "Agent registration token: " GOCDNEXT_AGENT_TOKEN; echo
export GOCDNEXT_AGENT_TOKEN
./gocdnext-agent-install-macos.sh
unset GOCDNEXT_AGENT_TOKEN
```

Set `GOCDNEXT_RUN_USER` if the service should run as an account other than the
user invoking the installer. The default tags are `macos,arm64` or
`macos,amd64`, based on the machine. Capacity defaults to 1 because Xcode
archives are resource-heavy.

The installer creates:

| Path | Purpose |
|---|---|
| `/usr/local/bin/gocdnext-agent` | Signed-checksum-verified native binary |
| `/Library/LaunchDaemons/io.gocdnext.agent.plist` | Root-owned service definition; contains no token |
| `/Library/Application Support/gocdnext/agent.env` | Agent settings and token, owned by the build account, mode `0600` |
| `/Library/Application Support/gocdnext/workspaces` | Default checkout/workspace root |
| `/Library/Application Support/gocdnext/logs` | Service stdout and stderr |

The LaunchDaemon is system-managed but its `UserName` is the unprivileged
build account. `GOCDNEXT_AGENT_ENGINE=shell` is pinned in `agent.env`; no job
container is involved.

## Verify registration and routing

Inspect the service and its logs:

```bash
sudo launchctl print system/io.gocdnext.agent
tail -f "/Library/Application Support/gocdnext/logs/agent.log"
```

The agent should appear under **Agents** as idle, with its release version and
the `macos` plus architecture tags. A minimal routing smoke is:

```yaml title=".gocdnext/mac-smoke.yaml"
name: mac-smoke
when:
  event: [manual]
stages: [verify]
jobs:
  host:
    stage: verify
    tags: [macos]
    script:
      - echo ok
      - sw_vers
```

Do not add `uses:` to a Mac host job: plugins are OCI images and the shell
engine deliberately rejects them. Native iOS work uses `script:`; a later
Linux job can consume the resulting `.ipa` artifact and run a deploy plugin.

## Upgrade, reconfigure, or remove

Re-run the installer with a new `GOCDNEXT_VERSION` or settings. It replaces the
binary and configuration atomically enough for `launchd`, then restarts the
service. Keep the same token for an already-registered name unless you also
rotate that agent's stored token server-side.

To remove the service and credentials while retaining existing workspaces:

```bash
sudo launchctl bootout system/io.gocdnext.agent
sudo rm -f /Library/LaunchDaemons/io.gocdnext.agent.plist \
  /usr/local/bin/gocdnext-agent \
  /usr/local/libexec/gocdnext-agent-wrapper \
  "/Library/Application Support/gocdnext/agent.env"
```

Delete `/Library/Application Support/gocdnext/workspaces` separately only when
you are certain no build output there is needed.

## Log growth

`launchd` writes the agent's stdout/stderr to
`/Library/Application Support/gocdnext/logs/agent.log` and `agent.err.log`.
These grow unbounded — the installer does **not** rotate them.

Rotating them is **not** a simple drop-in `newsyslog` rule: `launchd` holds
`StandardOutPath`/`StandardErrorPath` as open file descriptors, so renaming the
file on disk does not make the running agent reopen the path — it keeps writing
to the archived inode. Any rotation must make the service reopen its log files,
e.g. truncate/rotate during a maintenance window and then restart the service so
`launchd` reopens the paths:

```bash
sudo launchctl kickstart -k system/io.gocdnext.agent
```

A validated rotation recipe will be added after the real-Mac smoke confirms the
reopen behaviour. Until then, size the disk accordingly or truncate the logs
during a drain window.

## Stopping and draining

`KeepAlive` is set to `SuccessfulExit=false`: the daemon restarts on a crash or
lost connection, but a clean exit `0` (graceful drain on `SIGTERM`, see agent
drain) leaves it stopped. To stop it deliberately, `bootout` the service rather
than killing the process — a plain `kill` that produces a non-zero exit would be
relaunched:

```bash
sudo launchctl bootout system/io.gocdnext.agent
```
