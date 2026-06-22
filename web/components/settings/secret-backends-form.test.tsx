import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import type { SecretBackend, SecretBackendProbeResult } from "@/types/api";
import { SecretBackendsForm } from "./secret-backends-form.client";
import { selectOption } from "@/test/select";

// Server actions mocked at module level — the form dispatches them on
// save/test/delete; a unit test must never fire a real fetch. Each mock
// takes the action's single input arg so `.mock.calls[i][0]` is the
// dispatched payload we assert on.
// Default response mirrors a real save with no credential: the server returns
// credential_keys as null (Go nil []string), which is exactly the shape that
// used to crash the panel. Tests that assert a stored credential override this.
const setSecretBackend = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
  data: {
    source: "gcp",
    enabled: true,
    value: {},
    credential_keys: null,
    source_origin: "db",
  } satisfies SecretBackend,
}));
const deleteSecretBackend = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
  data: undefined,
}));
const testSecretBackend = vi.fn(
  async (
    _input: Record<string, unknown>,
  ): Promise<{ ok: true; probe: SecretBackendProbeResult } | { ok: false; error: string }> => ({
    ok: true,
    probe: { status: "ok", message: "reachable" },
  }),
);

vi.mock("@/server/actions/secret-backends", () => ({
  setSecretBackend: (input: Record<string, unknown>) => setSecretBackend(input),
  deleteSecretBackend: (input: Record<string, unknown>) =>
    deleteSecretBackend(input),
  testSecretBackend: (input: Record<string, unknown>) =>
    testSecretBackend(input),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}));

function backend(over: Partial<SecretBackend> & Pick<SecretBackend, "source">): SecretBackend {
  return {
    enabled: false,
    value: {},
    credential_keys: [],
    source_origin: "env",
    ...over,
  };
}

const threeBackends: SecretBackend[] = [
  backend({ source: "vault" }),
  backend({ source: "gcp" }),
  backend({ source: "aws" }),
];

beforeEach(() => {
  setSecretBackend.mockClear();
  deleteSecretBackend.mockClear();
  testSecretBackend.mockClear();
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.error).mockClear();
});

// panel returns the section for a given backend display name, scoped so
// queries don't bleed across the three panels.
function panel(name: RegExp) {
  return within(screen.getByRole("region", { name }));
}

describe("SecretBackendsForm rendering", () => {
  it("renders one panel per backend", () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    expect(screen.getByRole("region", { name: /HashiCorp Vault/ })).toBeTruthy();
    expect(screen.getByRole("region", { name: /GCP Secret Manager/ })).toBeTruthy();
    expect(screen.getByRole("region", { name: /AWS Secrets Manager/ })).toBeTruthy();
  });

  it("reveals Vault connection + credential fields when enabled", () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/HashiCorp Vault/);
    // Collapsed while disabled.
    expect(p.queryByLabelText(/Vault address/)).toBeNull();
    fireEvent.click(p.getByLabelText(/Enable HashiCorp Vault/));
    expect(p.getByLabelText(/Vault address/)).toBeTruthy();
    expect(p.getByLabelText(/Auth method/)).toBeTruthy();
    // approle is the default → role_id + secret_id credential visible.
    expect(p.getByLabelText(/Role ID/)).toBeTruthy();
    expect(p.getByLabelText(/AppRole secret_id/i)).toBeTruthy();
  });

  it("swaps the credential fields when the Auth method select changes", async () => {
    const user = userEvent.setup();
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/HashiCorp Vault/);
    fireEvent.click(p.getByLabelText(/Enable HashiCorp Vault/));

    // approle default → role_id present, no Vault role / Token.
    expect(p.getByLabelText(/Role ID/)).toBeTruthy();
    expect(p.queryByLabelText(/Vault role/)).toBeNull();

    await selectOption(user, p.getByLabelText(/Auth method/), "Kubernetes");
    expect(p.getByLabelText(/Vault role/)).toBeTruthy();
    expect(p.queryByLabelText(/Role ID/)).toBeNull();

    await selectOption(user, p.getByLabelText(/Auth method/), "Token");
    expect(p.getByLabelText(/^Token/)).toBeTruthy();
    expect(p.queryByLabelText(/Vault role/)).toBeNull();
  });

  it("shows a 'from env' badge for env origin and no delete button", () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/HashiCorp Vault/);
    expect(p.getByText(/from env/i)).toBeTruthy();
    expect(p.queryByRole("button", { name: /Delete HashiCorp Vault/ })).toBeNull();
  });

  it("shows Delete only for a db-origin backend", () => {
    render(
      <SecretBackendsForm
        initial={[
          backend({
            source: "vault",
            enabled: true,
            source_origin: "db",
            value: { addr: "https://vault.example.com", auth: "approle", role_id: "r1" },
          }),
          backend({ source: "gcp" }),
          backend({ source: "aws" }),
        ]}
      />,
    );
    expect(
      panel(/HashiCorp Vault/).getByRole("button", { name: /Delete HashiCorp Vault/ }),
    ).toBeTruthy();
    expect(panel(/GCP Secret Manager/).getByText(/from env/i)).toBeTruthy();
  });

  it("renders a '•••• stored' badge when a credential is configured", () => {
    render(
      <SecretBackendsForm
        initial={[
          backend({
            source: "vault",
            enabled: true,
            source_origin: "db",
            credential_keys: ["configured"],
            value: { addr: "https://vault.example.com", auth: "approle", role_id: "r1" },
          }),
          backend({ source: "gcp" }),
          backend({ source: "aws" }),
        ]}
      />,
    );
    expect(panel(/HashiCorp Vault/).getByText(/•••• stored/)).toBeTruthy();
  });
});

describe("SecretBackendsForm dispatch", () => {
  it("saves Vault with credentials present and preserve_credentials false on first save", async () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/HashiCorp Vault/);
    fireEvent.click(p.getByLabelText(/Enable HashiCorp Vault/));
    fireEvent.change(p.getByLabelText(/Vault address/), {
      target: { value: "https://vault.example.com" },
    });
    fireEvent.change(p.getByLabelText(/Role ID/), {
      target: { value: "ci-role" },
    });
    fireEvent.change(p.getByLabelText(/AppRole secret_id/i), {
      target: { value: "s3cr3t-id" },
    });

    fireEvent.click(p.getByRole("button", { name: /Save HashiCorp Vault/ }));

    await waitFor(() => expect(setSecretBackend).toHaveBeenCalledTimes(1));
    const arg = setSecretBackend.mock.calls[0]![0];
    expect(arg.source).toBe("vault");
    expect(arg.enabled).toBe(true);
    expect(arg.value).toMatchObject({
      addr: "https://vault.example.com",
      auth: "approle",
      role_id: "ci-role",
    });
    expect(arg.credentials).toEqual({ secret_id: "s3cr3t-id" });
    expect(arg.preserve_credentials).toBe(false);
  });

  it("sends preserve_credentials true and no credentials when editing without retyping", async () => {
    render(
      <SecretBackendsForm
        initial={[
          backend({
            source: "vault",
            enabled: true,
            source_origin: "db",
            credential_keys: ["configured"],
            value: { addr: "https://vault.example.com", auth: "approle", role_id: "ci-role" },
          }),
          backend({ source: "gcp" }),
          backend({ source: "aws" }),
        ]}
      />,
    );
    const p = panel(/HashiCorp Vault/);
    // Leave the credential blank → preserve the stored one.
    fireEvent.click(p.getByRole("button", { name: /Save HashiCorp Vault/ }));

    await waitFor(() => expect(setSecretBackend).toHaveBeenCalledTimes(1));
    const arg = setSecretBackend.mock.calls[0]![0];
    expect(arg.preserve_credentials).toBe(true);
    expect(arg.credentials).toBeUndefined();
  });

  it("does NOT preserve an env-origin credential (can't copy env into a DB override)", async () => {
    // Env-origin Vault reports a credential configured (from env). Saving a DB
    // override without retyping must NOT send preserve_credentials — there's no
    // stored DB credential to preserve; the server would reject it. The user
    // is prompted to re-enter (a hint is shown).
    render(
      <SecretBackendsForm
        initial={[
          backend({
            source: "vault",
            enabled: true,
            source_origin: "env",
            credential_keys: ["configured"],
            value: { addr: "https://vault.example.com", auth: "approle", role_id: "ci-role" },
          }),
          backend({ source: "gcp" }),
          backend({ source: "aws" }),
        ]}
      />,
    );
    const p = panel(/HashiCorp Vault/);
    expect(p.getByText(/comes from the environment/i)).toBeTruthy();
    fireEvent.click(p.getByRole("button", { name: /Save HashiCorp Vault/ }));

    await waitFor(() => expect(setSecretBackend).toHaveBeenCalledTimes(1));
    const arg = setSecretBackend.mock.calls[0]![0];
    expect(arg.preserve_credentials).toBeUndefined();
    expect(arg.credentials).toBeUndefined();
  });

  it("saves GCP with project and no credentials field", async () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/GCP Secret Manager/);
    fireEvent.click(p.getByLabelText(/Enable GCP Secret Manager/));
    fireEvent.change(p.getByLabelText(/GCP project/), {
      target: { value: "my-project" },
    });

    fireEvent.click(p.getByRole("button", { name: /Save GCP Secret Manager/ }));

    await waitFor(() => expect(setSecretBackend).toHaveBeenCalledTimes(1));
    const arg = setSecretBackend.mock.calls[0]![0];
    expect(arg.source).toBe("gcp");
    expect(arg.value).toMatchObject({ project: "my-project" });
    expect(arg.credentials).toBeUndefined();
    expect(arg.preserve_credentials).toBeUndefined();
    // Regression: the server returns credential_keys: null for a no-credential
    // save. The handler must complete (success toast) rather than throw on
    // null.length. (vi.mock("sonner") makes toast.success a spy.)
    await waitFor(() => expect(toast.success).toHaveBeenCalled());
  });

  it("saves AWS with region and no credentials field", async () => {
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/AWS Secrets Manager/);
    fireEvent.click(p.getByLabelText(/Enable AWS Secrets Manager/));
    fireEvent.change(p.getByLabelText(/AWS region/), {
      target: { value: "us-east-1" },
    });

    fireEvent.click(p.getByRole("button", { name: /Save AWS Secrets Manager/ }));

    await waitFor(() => expect(setSecretBackend).toHaveBeenCalledTimes(1));
    const arg = setSecretBackend.mock.calls[0]![0];
    expect(arg.source).toBe("aws");
    expect(arg.value).toMatchObject({ region: "us-east-1" });
    expect(arg.credentials).toBeUndefined();
  });

  it("renders the probe status from Test connection", async () => {
    testSecretBackend.mockResolvedValueOnce({
      ok: true,
      probe: { status: "unauthorized", message: "bad role_id" },
    });
    render(<SecretBackendsForm initial={threeBackends} />);
    const p = panel(/HashiCorp Vault/);
    fireEvent.click(p.getByLabelText(/Enable HashiCorp Vault/));
    fireEvent.change(p.getByLabelText(/Vault address/), {
      target: { value: "https://vault.example.com" },
    });
    fireEvent.change(p.getByLabelText(/Role ID/), {
      target: { value: "ci-role" },
    });

    fireEvent.click(p.getByRole("button", { name: /Test HashiCorp Vault/ }));

    await waitFor(() => expect(testSecretBackend).toHaveBeenCalledTimes(1));
    expect(await p.findByText(/bad role_id/)).toBeTruthy();
    expect(p.getByText(/unauthorized/i)).toBeTruthy();
  });
});
