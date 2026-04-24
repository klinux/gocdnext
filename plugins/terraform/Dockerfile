# gocdnext/terraform — run terraform CLI commands (init, plan,
# apply, fmt, validate, destroy, output). Backend credentials
# come via env — pipe AWS_*, GOOGLE_CREDENTIALS, ARM_*, etc via
# the job's `secrets:` list so nothing lands in the log.
#
# This plugin is a shell-thin wrapper around `terraform`. For
# apply-with-approval flows, chain it with an approval-gate job
# upstream: plan → approval → apply.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND     (required)  terraform subcommand + args,
#                                  word-split. Examples:
#                                    "init -input=false"
#                                    "plan -out=tfplan"
#                                    "apply -input=false tfplan"
#                                    "fmt -check -recursive"
#                                    "validate"
#                                    "destroy -auto-approve"
#   PLUGIN_WORKING_DIR (optional)  dir under /workspace to cd into.
#                                  Default ".".
#   PLUGIN_VERSION     (optional)  terraform version to install via
#                                  tfenv. Default "latest" (use
#                                  the version pinned in the image).
#                                  Pin to a specific version
#                                  (e.g. "1.9.8") when the module
#                                  needs deterministic behaviour.
#   PLUGIN_VAR_FILE    (optional)  path to a -var-file (passed to
#                                  init/plan/apply automatically
#                                  when set; ignored for other
#                                  subcommands).
FROM hashicorp/terraform:1.9.8

RUN apk add --no-cache bash git

COPY entrypoint.sh /usr/local/bin/gocdnext-terraform
RUN chmod +x /usr/local/bin/gocdnext-terraform

WORKDIR /workspace

# The upstream image sets ENTRYPOINT=["/bin/terraform"] — reset
# so our wrapper runs first. The wrapper calls terraform itself.
ENTRYPOINT ["/usr/local/bin/gocdnext-terraform"]
