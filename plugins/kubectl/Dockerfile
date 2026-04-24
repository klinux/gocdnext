# gocdnext/kubectl — apply manifests / rollout / generic kubectl.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND    (required)  kubectl subcommand + args, word-split.
#                                 Examples: "apply -f k8s/",
#                                 "rollout status deploy/api -n prod",
#                                 "get pods -o wide".
#   PLUGIN_KUBECONFIG (optional)  path (relative to /workspace) OR base64
#                                 inline config. If set, gets written to
#                                 /tmp/kubeconfig and KUBECONFIG points
#                                 at it. Empty = rely on the cluster's
#                                 in-pod service account (when the agent
#                                 runs in-cluster).
#   PLUGIN_NAMESPACE  (optional)  default namespace via `--namespace`.
#
# Operators who need auth via cloud-provider plugins
# (aws-iam-authenticator / gke-gcloud-auth-plugin) should start
# with this image as a base and layer those tools in — the MVP
# plugin stays minimal to keep the image small.
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl \
    && curl -sSL -o /usr/local/bin/kubectl \
        "https://dl.k8s.io/release/v1.31.1/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl

COPY entrypoint.sh /usr/local/bin/gocdnext-kubectl
RUN chmod +x /usr/local/bin/gocdnext-kubectl

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-kubectl"]
