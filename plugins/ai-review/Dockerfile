# gocdnext/ai-review — LLM-driven code review for trunk-based
# PR pipelines. Sends the PR diff to Anthropic (Claude) or
# OpenAI (GPT/Codex), parses structured findings, and optionally
# posts them as a GitHub/GitLab PR comment.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PROVIDER          (required) claude | openai
#   PLUGIN_MODEL             (optional) override; defaults per
#                                       provider (Claude:
#                                       claude-sonnet-4-6,
#                                       OpenAI: gpt-4o-mini).
#   PLUGIN_BASE_REF          (optional) git ref to diff against;
#                                       default `origin/main`.
#   PLUGIN_HEAD_REF          (optional) default HEAD.
#   PLUGIN_MODE              (optional) console | pr-comment;
#                                       default console.
#   PLUGIN_PR_NUMBER         (required for mode=pr-comment)
#   PLUGIN_REPO              (required for mode=pr-comment)
#                                       GitHub: owner/repo.
#                                       GitLab: numeric project id
#                                       or url-encoded namespace.
#   PLUGIN_SCM_PROVIDER      (optional) github | gitlab; default
#                                       github.
#   PLUGIN_SYSTEM_PROMPT     (optional) override the default
#                                       reviewer prompt.
#   PLUGIN_TEMPERATURE       (optional) default "0".
#   PLUGIN_MAX_TOKENS        (optional) default "4096".
#   PLUGIN_MAX_DIFF_BYTES    (optional) cap on the diff length
#                                       sent to the model (cost
#                                       guardrail). Default 50000
#                                       (~12k tokens). Diff is
#                                       truncated with a warning
#                                       when exceeded.
#   PLUGIN_DIFF_EXCLUDE      (optional) `:(exclude)` pathspecs
#                                       for `git diff`. Default
#                                       skips vendored deps and
#                                       lockfiles.
#   PLUGIN_SEVERITY_THRESHOLD (optional) info | warning | error;
#                                       filters findings below
#                                       the threshold from the
#                                       output. Default warning.
#   PLUGIN_FAIL_ON_ERROR     (optional) true|false (bool); when
#                                       true, exit non-zero if
#                                       any error-severity
#                                       finding survives the
#                                       threshold filter — gates
#                                       PR merge. Default false.
#
# Secrets (job's `secrets:` list, never on argv):
#   ANTHROPIC_API_KEY  (when PROVIDER=claude)
#   OPENAI_API_KEY     (when PROVIDER=openai)
#   GITHUB_TOKEN       (when MODE=pr-comment + scm=github)
#   GITLAB_TOKEN       (when MODE=pr-comment + scm=gitlab)
#
# Cost guardrails: the entrypoint enforces MAX_DIFF_BYTES BEFORE
# the API call so a 500 KB monorepo PR doesn't surprise the team
# with a $10 bill. Audit trail: full request + response saved to
# .ai-review/ for `artifacts: optional: [.ai-review]`.
FROM alpine:3.20

RUN apk add --no-cache bash curl jq git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-ai-review
RUN chmod +x /usr/local/bin/gocdnext-ai-review

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-ai-review"]
