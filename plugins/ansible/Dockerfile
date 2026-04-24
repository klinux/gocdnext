# gocdnext/ansible — run playbooks against managed hosts. Thin
# wrapper around `ansible-playbook` with credential handling
# wired through `secrets:` (private SSH key, become-password,
# vault-password). Useful for deploys to bare-metal, VMs, and
# anything outside Kubernetes where kubectl/helm don't apply.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PLAYBOOK        (required)  path (under workspace) to
#                                      the playbook, e.g.
#                                      "deploy.yml".
#   PLUGIN_INVENTORY       (optional)  path to inventory. Defaults
#                                      to ansible's fallback
#                                      (/etc/ansible/hosts → none).
#   PLUGIN_EXTRA_VARS      (optional)  -e VALUES. Accepts raw
#                                      "foo=bar baz=qux" OR a path
#                                      to a YAML/JSON file via
#                                      "@path/to/vars.yml".
#   PLUGIN_TAGS            (optional)  -t tag1,tag2 filter.
#   PLUGIN_LIMIT           (optional)  -l host pattern filter.
#   PLUGIN_SSH_PRIVATE_KEY (optional)  PEM key body (piped via
#                                      secrets). Written to a
#                                      0600 tempfile and passed
#                                      via --private-key.
#   PLUGIN_SSH_USER        (optional)  --user (remote ssh user).
#   PLUGIN_BECOME_PASSWORD (optional)  sudo password. Passed via
#                                      --extra-vars ansible_become_password=...
#                                      to avoid a prompt.
#   PLUGIN_VAULT_PASSWORD  (optional)  vault password for
#                                      encrypted files. Written
#                                      to tempfile, passed via
#                                      --vault-password-file.
#   PLUGIN_CHECK           (optional)  "true" → --check (dry run).
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash git openssh-client sshpass ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir ansible-core==2.17.6 ansible==10.6.0

COPY entrypoint.sh /usr/local/bin/gocdnext-ansible
RUN chmod +x /usr/local/bin/gocdnext-ansible

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-ansible"]
