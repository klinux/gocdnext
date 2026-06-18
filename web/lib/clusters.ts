// Shared cluster constants + types. Kept out of the "use server"
// actions module because a server-actions file may only export async
// functions — Next strips any const/type export, which breaks client
// imports. Both the actions and the client manager import from here.

// auth_type drives which credential shape the server expects:
//   kubeconfig  → credential carries the full kubeconfig YAML
//   token       → api_server + ca_cert + a bearer token in credential
//   in_cluster  → no credential at all (the agent uses its ServiceAccount)
export const clusterAuthTypes = ["kubeconfig", "token", "in_cluster"] as const;
export type ClusterAuthType = (typeof clusterAuthTypes)[number];

// Sentinel the server understands on PUT to mean "keep the stored
// credential ciphertext — the admin left the write-only field blank".
// Mirrors the runner-profiles SECRET_PRESERVE pattern so the
// never-echo-plaintext rule holds: the value is entered on create /
// rotate, never read back.
export const CREDENTIAL_PRESERVE_SENTINEL = "__GOCDNEXT_SECRET_PRESERVE__";
