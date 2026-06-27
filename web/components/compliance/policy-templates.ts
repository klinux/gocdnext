// Starter templates for a new compliance policy — each pre-fills the config
// editor with a ready-to-tweak `_compliance_*` fragment wired to one of the
// shipped security plugins (or an approval gate). Everything stays editable;
// nothing is enforced until the admin saves. (#72 part A)

export type PolicyTemplate = {
  key: string;
  label: string;
  description: string;
  mode?: "inject" | "override";
  configYaml: string;
};

export const POLICY_TEMPLATES: PolicyTemplate[] = [
  {
    key: "sast",
    label: "SAST — semgrep",
    description: "Static analysis on every targeted project (OWASP Top Ten).",
    configYaml: `stages: [_compliance_sast]
jobs:
  _compliance_sast:
    stage: _compliance_sast
    uses: ghcr.io/klinux/gocdnext-plugin-semgrep@v1
    with:
      config: p/owasp-top-ten
`,
  },
  {
    key: "sca",
    label: "Dependency scan — osv-scanner",
    description: "Flags known-vulnerable dependencies in the repo.",
    configYaml: `stages: [_compliance_sca]
jobs:
  _compliance_sca:
    stage: _compliance_sca
    uses: ghcr.io/klinux/gocdnext-plugin-osv-scanner@v1
`,
  },
  {
    key: "secrets",
    label: "Secret scan — gitleaks",
    description: "Detects committed secrets before they reach a build.",
    configYaml: `stages: [_compliance_secrets]
jobs:
  _compliance_secrets:
    stage: _compliance_secrets
    uses: ghcr.io/klinux/gocdnext-plugin-gitleaks@v1
`,
  },
  {
    key: "sign",
    label: "Image signing — cosign",
    description: "Signs the published image. Fill in your image ref.",
    configYaml: `stages: [_compliance_sign]
jobs:
  _compliance_sign:
    stage: _compliance_sign
    uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
    with:
      # Replace with the image your project publishes (a digest ref is best).
      image: registry.example.com/app:latest
      action: sign
`,
  },
  {
    key: "signoff",
    label: "Sign-off gate — approval",
    description: "Separation-of-duties approval before release.",
    configYaml: `stages: [_compliance_signoff]
jobs:
  _compliance_signoff:
    stage: _compliance_signoff
    approval:
      description: Separation-of-duties sign-off
      required: 1
`,
  },
];
