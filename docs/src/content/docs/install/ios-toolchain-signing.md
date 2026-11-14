---
title: iOS toolchain and signing
description: Prepare a macOS agent with Xcode, Fastlane, CocoaPods and ephemeral code signing.
---

This runbook starts after the [native macOS agent](/gocdnext/docs/install/macos-agent/)
is registered. It prepares that unprivileged build account for native iOS jobs
and configures `fastlane match` without putting signing material in the
workspace, command arguments, or committed files.

## Install and select Xcode

Install the full Xcode application from Apple on the Mac first. Do this as an
operator; the bootstrap deliberately does not download Xcode or accept a new
license during an ordinary pipeline job.

As the same account configured in the agent's LaunchDaemon, check out the
gocdnext release you deploy and run:

```bash
export GOCDNEXT_XCODE_DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
scripts/macos/bootstrap-ios.sh
```

The bootstrap is idempotent. On its first run it uses `sudo` only to select the
requested Xcode and complete Apple's license/first-launch setup. It then uses
Homebrew to install `ruby@3.4`, and installs pinned Bundler, Fastlane and
CocoaPods gems under the build user's
`~/.local/share/gocdnext-ios` directory. It never changes the macOS system Ruby.

The generated `~/.config/gocdnext/ios-toolchain.env` is mode `0600`. Native
jobs source it to get the selected Ruby and tools:

```bash
source "$HOME/.config/gocdnext/ios-toolchain.env"
xcodebuild -version
ruby --version
bundle --version
bundle exec fastlane --version
bundle exec pod --version
```

Commit a `Gemfile` and `Gemfile.lock` to the app repository so updates happen
in review, not because a host fetched a newer transitive dependency. The
complete pinned example lives in `examples/ios-signing/`.

To install a second Xcode side by side, rerun the bootstrap with its developer
directory. A pipeline may override `DEVELOPER_DIR` before sourcing the env file;
the bootstrap value is only the default.

## Prepare signing outside CI

Create these once from a trusted administrator workstation:

1. An App Store Connect **team API key** with the provisioning access your
   application needs. Keep its `.p8`, key ID, and issuer ID.
2. A private `match` Git repository. Initialize certificates and provisioning
   profiles there with `fastlane match` from the trusted workstation; CI uses
   `readonly: true` and must not create or rotate them.
3. A dedicated read-only credential for that repository. The example uses
   Fastlane's `MATCH_GIT_BASIC_AUTHORIZATION` environment variable for HTTPS.
   Store the base64-encoded `username:token` value as a secret. A dedicated SSH
   deploy key is also possible, but do not place it in the workspace.

Add the following project secrets in gocdnext:

| Secret | Value |
|---|---|
| `ASC_KEY_ID` | App Store Connect team key ID |
| `ASC_ISSUER_ID` | App Store Connect issuer UUID |
| `ASC_KEY_P8_BASE64` | Base64 of the complete `.p8` file |
| `MATCH_PASSWORD` | Passphrase that encrypts the match repository |
| `MATCH_GIT_BASIC_AUTHORIZATION` | Base64 `username:token` for the private repository |

On macOS, encode the key without line wrapping:

```bash
base64 < AuthKey_ABC123.p8 | tr -d '\n'
```

:::caution
Leave **Expose to pull requests** off for every signing secret. Keep the
project's same-repository PR-head configuration off for signing pipelines too.
A contributor who controls pipeline code can print, copy, or misuse any secret
made available to that run, even when normal log masking is active.
:::

## Sync into an ephemeral keychain

Copy the example `Gemfile`, lockfile, `fastlane/Fastfile`, and
`scripts/sync-signing.sh` into the app repository. Set `MATCH_GIT_URL` to the
private signing repository and use the included manual-only pipeline as the
first real-Mac smoke.

The wrapper:

- receives all five credentials through the job environment;
- decodes the `.p8` to a mode-`0600` file in `TMPDIR`, outside the workspace;
- generates a per-run keychain password in memory;
- invokes only `bundle exec fastlane ios sync_signing` in argv;
- asks `match` for App Store profiles in read-only mode; and
- deletes both the temporary keychain and key file on success, failure, or a
  handled termination.

The `Fastfile` passes the key file to `app_store_connect_api_key`, creates an
unlocked keychain with a one-hour timeout, and gives that keychain explicitly
to `match`. The API private key, match passphrase, repository credential, and
keychain password never appear in command arguments.

Run the pipeline manually and confirm:

1. the job is routed to an idle agent tagged `macos`;
2. `match` resolves the expected distribution certificate and profile;
3. the job exits successfully and `security list-keychains` shows no leftover
   `gocdnext-*.keychain-db`; and
4. searching the persisted and live logs for known fragments of each secret
   returns nothing.

This smoke requires a real Mac plus Apple credentials and cannot be represented
honestly by Linux CI. The repository CI instead exercises the bootstrap and
secret-materialization contracts hermetically.
