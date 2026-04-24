# gocdnext/aws-cli — run AWS CLI v2 commands. Credentials piped
# via `secrets:` (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY +
# optional AWS_SESSION_TOKEN). For OIDC-based federation (IAM
# role assumed from a Workload Identity-style flow) set
# AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE and the CLI picks
# it up automatically.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND (required)  aws subcommand + args, word-split.
#                              Examples:
#                                "s3 cp dist/ s3://bkt/ --recursive"
#                                "ecr describe-repositories"
#                                "ssm get-parameter --name /foo"
#                                "cloudformation deploy --stack-name X ..."
#   PLUGIN_REGION  (optional)  -- sets AWS_DEFAULT_REGION. Most
#                              commands need this; set it once
#                              here instead of threading --region
#                              through every invocation.
#   PLUGIN_PROFILE (optional)  --profile NAME. Rarely needed in
#                              CI — env-based creds cover the
#                              common case.
#   PLUGIN_WORKING_DIR (optional) cd here first. Default ".".
#
# AWS_* secrets go via the standard env vars — the runner already
# masks them in logs from the secrets: declaration.
FROM public.ecr.aws/aws-cli/aws-cli:2.22.8

# upstream awscli image ENTRYPOINT = /usr/local/bin/aws. Override
# so our wrapper owns the lifecycle (working-dir + region export).
ENTRYPOINT []

# Busybox-style shell exists in the base, but bash is more
# predictable for our entrypoint and matches the rest of the
# plugin stable.
RUN yum install -y bash && yum clean all

COPY entrypoint.sh /usr/local/bin/gocdnext-aws
RUN chmod +x /usr/local/bin/gocdnext-aws

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-aws"]
