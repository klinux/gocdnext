# gocdnext/maven-central-publish — upload a deployment bundle to
# Maven Central via the Sonatype Central Portal API.
#
# Contract:
#   CENTRAL_TOKEN        (required)  the portal's Bearer token
#                                    (base64 of user:password
#                                    generated at central.sonatype.com)
#                                    via the job's `secrets:`.
#   PLUGIN_BUNDLE        (required)  path to the bundle zip.
#   PLUGIN_PUBLISHING_TYPE (optional) AUTOMATIC | USER_MANAGED.
#   PLUGIN_WAIT          (optional)  poll until VALIDATED/PUBLISHED.
#
# Scope: this plugin UPLOADS a pre-built bundle. Producing it —
# jars + sources + javadoc + poms + .asc signatures + checksums in
# the Central layout — is the BUILD job's responsibility (the
# maven-publish/central-publishing plugins of Maven/Gradle output
# exactly this zip). GPG signing stays in the build job where the
# key lives; this container never sees private key material.
FROM alpine:3.21

RUN apk add --no-cache curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-maven-central-publish
RUN chmod +x /usr/local/bin/gocdnext-maven-central-publish

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-maven-central-publish"]
