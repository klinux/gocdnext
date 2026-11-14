---
title: iOS (Xcode and Fastlane)
description: Test, sign and archive an iOS app on a native macOS agent.
---

iOS compilation runs as a native `script:` job on an agent tagged `macos`.
Xcode is not containerized, and gocdnext plugins remain Linux OCI images. This
recipe uses Fastlane as the project-owned build interface: `scan` runs tests and
`gym` archives the app into an `.ipa` plus dSYMs.

Complete the [macOS agent](/gocdnext/docs/install/macos-agent/) and
[iOS toolchain/signing](/gocdnext/docs/install/ios-toolchain-signing/)
runbooks first. The full copyable project is in `examples/ios-build/`.

## Project convention

Commit these files with the app:

```text
.gocdnext/ios.yaml
Gemfile
Gemfile.lock
fastlane/Fastfile
scripts/ios-build.sh
```

The lockfile pins Fastlane, CocoaPods, and all transitive gems for both Apple
Silicon and Intel Darwin. The host bootstrap selects a non-system Ruby; the
project lock selects the build tools.

Set project-specific values in the pipeline:

```yaml
variables:
  DEVELOPER_DIR: /Applications/Xcode_18.1.app/Contents/Developer
  IOS_WORKSPACE: MobileApp.xcworkspace
  IOS_SCHEME: MobileApp
  IOS_BUNDLE_ID: com.acme.mobile
  IOS_SIMULATOR_DEVICE: iPhone 16 Pro
  IOS_EXPORT_METHOD: app-store
  IOS_OUTPUT_NAME: MobileApp.ipa
  MATCH_GIT_URL: https://github.com/acme/ios-signing.git
```

`DEVELOPER_DIR` selects an Xcode installed side by side for this job only. It
does not mutate machine-wide `xcode-select`, so two app pipelines can migrate
between Xcode versions independently.

## Test on Simulator

The test job has no signing credentials and cannot run a plugin image:

```yaml
jobs:
  test:
    stage: test
    tags: [macos]
    script:
      - source "$HOME/.config/gocdnext/ios-toolchain.env"
      - bundle config set --local path .bundle-cache
      - bundle check || bundle install --jobs 4 --retry 3
      - if [ -f Podfile ]; then bundle exec pod install; fi
      - bundle exec fastlane test
    test_reports: [build/test-results/junit.xml]
    artifacts:
      paths: [build/test-results/]
```

The example `test` lane calls `scan`, writes JUnit to a deterministic path, and
keeps its result bundle under `build/test-results/` for diagnosis.

## Produce the signed archive

The archive job runs only after the test stage succeeds. It declares exactly
the signing secrets it needs and is restricted to `push`/`manual` on `main`:

```yaml
archive:
  stage: archive
  tags: [macos]
  secrets:
    - ASC_KEY_ID
    - ASC_ISSUER_ID
    - ASC_KEY_P8_BASE64
    - MATCH_PASSWORD
    - MATCH_GIT_BASIC_AUTHORIZATION
  script:
    - source "$HOME/.config/gocdnext/ios-toolchain.env"
    - bundle config set --local path .bundle-cache
    - bundle check || bundle install --jobs 4 --retry 3
    - if [ -f Podfile ]; then bundle exec pod install; fi
    - ./scripts/ios-build.sh
  artifacts:
    paths:
      - build/artifacts/*.ipa
      - build/artifacts/*.app.dSYM.zip
```

The wrapper only owns secret materialization and cleanup. Its build operation is
exactly:

```bash
bundle exec fastlane build
```

The `build` lane creates a temporary keychain, runs read-only `match`, and calls
`gym`. It then asserts that Fastlane returned both an IPA path and a dSYM zip;
missing symbols fail the job rather than silently publishing an incomplete
artifact set. An `ensure` block removes the keychain, while the wrapper removes
the mode-`0600` `.p8` outside the workspace and provides a second keychain
cleanup attempt on exit.

:::caution
Do not add `pull_request` to the signed archive pipeline and do not expose its
five secrets to PRs. Run PR tests in a separate, unsigned Simulator pipeline if
contributors need feedback before merge.
:::

## Real-Mac acceptance

Before calling the setup production-ready, run the example against the actual
app and verify:

1. routing selects the intended `macos` agent and `xcodebuild -version` matches
   `DEVELOPER_DIR`;
2. `scan` executes the expected test plan and gocdnext ingests its JUnit file;
3. `match` resolves the expected App Store profile without changing its repo;
4. `gym` produces a signed `.ipa` and `*.app.dSYM.zip`, both downloadable from
   the run's artifact list; and
5. no signing value appears in live or persisted logs, and no temporary
   keychain remains after either a successful or deliberately failed archive.

The repository tests validate the YAML, Fastfile syntax, argv and cleanup
contracts on Linux. They cannot substitute for this Xcode/Apple smoke.
