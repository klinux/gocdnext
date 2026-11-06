# gocdnext/terraform — run terraform CLI commands. The image keeps
# Terraform 1.17.1 as its stable default and bundles tfenv so the
# manifest's `version:` input can select an exact release at runtime.
FROM alpine:3.22 AS tfenv-source

ARG TFENV_VERSION=3.2.2
ARG TFENV_COMMIT=de6ce2e809c155cbc5e2cfeb3b1bef151244e045

RUN apk add --no-cache git \
    && git clone --depth 1 --branch "v${TFENV_VERSION}" \
       https://github.com/tfutils/tfenv.git /opt/tfenv \
    && test "$(git -C /opt/tfenv rev-parse HEAD)" = "${TFENV_COMMIT}" \
    && rm -rf /opt/tfenv/.git

FROM hashicorp/terraform:1.17.1

# tfenv verifies every downloaded Terraform archive against HashiCorp's
# published SHA-256 list. curl + unzip are its runtime dependencies.
RUN apk upgrade --no-cache \
    && apk add --no-cache bash curl git unzip

COPY --from=tfenv-source /opt/tfenv /opt/tfenv
COPY entrypoint.sh /usr/local/bin/gocdnext-terraform
RUN chmod +x /usr/local/bin/gocdnext-terraform

ENV TFENV_ROOT=/opt/tfenv

WORKDIR /workspace

# The upstream image sets ENTRYPOINT=["/bin/terraform"] — reset
# so our wrapper runs first.
ENTRYPOINT ["/usr/local/bin/gocdnext-terraform"]
