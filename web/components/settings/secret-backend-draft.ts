import type { SecretBackend } from "@/types/api";

// BackendDraft is the flat local editor state covering all three
// backends. Only the fields relevant to the selected source are read
// when building the wire payload, so the unused ones stay at their
// defaults harmlessly. Credentials (secretId/token) are write-only:
// they start blank on edit and a blank value means "preserve the stored
// credential".
export type BackendDraft = {
  enabled: boolean;
  // vault
  addr: string;
  auth: "approle" | "kubernetes" | "token";
  roleId: string;
  secretId: string;
  role: string;
  jwtPath: string;
  token: string;
  kvMount: string;
  namespace: string;
  caCert: string;
  insecureSkipVerify: boolean;
  // gcp
  project: string;
  // aws
  region: string;
  endpoint: string;
};

function str(v: Record<string, unknown>, key: string): string {
  const raw = v[key];
  return typeof raw === "string" ? raw : "";
}

function vaultAuth(v: Record<string, unknown>): BackendDraft["auth"] {
  const a = str(v, "auth");
  if (a === "kubernetes" || a === "token") return a;
  return "approle"; // default + unknown → approle
}

function bool(v: Record<string, unknown>, key: string): boolean {
  return v[key] === true;
}

// draftFrom seeds the editor from a server row. Non-secret value fields
// are prefilled; write-only credentials always start blank (the server
// never echoes them).
export function draftFrom(b: SecretBackend): BackendDraft {
  const v = b.value ?? {};
  return {
    enabled: b.enabled,
    addr: str(v, "addr"),
    auth: vaultAuth(v),
    roleId: str(v, "role_id"),
    secretId: "",
    role: str(v, "role"),
    jwtPath: str(v, "jwt_path"),
    token: "",
    kvMount: str(v, "kv_mount"),
    namespace: str(v, "namespace"),
    caCert: str(v, "ca_cert"),
    insecureSkipVerify: bool(v, "insecure_skip_verify"),
    project: str(v, "project"),
    region: str(v, "region"),
    endpoint: str(v, "endpoint"),
  };
}
