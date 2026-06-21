import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { SecretDialog } from "./secret-dialog.client";
import { openSelect, selectOption } from "@/test/select";

// Server actions mocked at module level — the dialog dispatches them
// on submit; a unit test must never fire a real fetch. Each mock takes
// the action's single input arg so `.mock.calls[i][0]` is the
// dispatched payload we assert on.
const setSecret = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
  created: true,
}));
const setGlobalSecret = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
  created: true,
}));

vi.mock("@/server/actions/secrets", () => ({
  setSecret: (input: Record<string, unknown>) => setSecret(input),
  setGlobalSecret: (input: Record<string, unknown>) => setGlobalSecret(input),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

beforeEach(() => {
  setSecret.mockClear();
  setGlobalSecret.mockClear();
});

// openDialog clicks the trigger and returns once the source selector
// (always present) is on screen. The selector is now a base-ui Select
// trigger (a button), so findByLabelText resolves the trigger button.
async function openDialog() {
  fireEvent.click(screen.getByRole("button", { name: /new secret/i }));
  return screen.findByLabelText("Source");
}

describe("SecretDialog source fields", () => {
  it("shows the value field for db and hides the path/key fields", async () => {
    render(<SecretDialog slug="demo" configuredSources={["db", "vault"]} />);
    await openDialog();

    // db is offered and first → the default source → value Textarea
    // present, no path/key.
    expect(screen.getByLabelText("Value")).toBeTruthy();
    expect(screen.queryByLabelText(/^Path/)).toBeNull();
    expect(screen.queryByLabelText(/^Key/)).toBeNull();
  });

  it("swaps to path/key for an external source and hides the value field", async () => {
    const user = userEvent.setup();
    render(<SecretDialog slug="demo" configuredSources={["db", "vault"]} />);
    const select = await openDialog();

    await selectOption(user, select, "HashiCorp Vault");

    expect(screen.queryByLabelText("Value")).toBeNull();
    expect(screen.getByLabelText(/^Path/)).toBeTruthy();
    // Vault → the Key field is required (carries a "*").
    expect(screen.getByLabelText(/^Key/)).toBeTruthy();
  });

  it("only offers backends the server reports as configured", async () => {
    const user = userEvent.setup();
    render(<SecretDialog slug="demo" configuredSources={["db", "gcp"]} />);
    const listbox = await openSelect(user, await openDialog());
    const names = within(listbox)
      .getAllByRole("option")
      .map((o) => o.textContent);
    // The server reported db + gcp; vault/aws are not configured.
    expect(names).toContain("Stored value");
    expect(names).toContain("GCP Secret Manager");
    expect(names).not.toContain("HashiCorp Vault");
    expect(names).not.toContain("AWS Secrets Manager");
  });

  it("omits db and defaults to the first backend on an external-only server", async () => {
    // No cipher → the server omits db from configured_sources; the
    // selector must not offer it (a db write would 503), and the dialog
    // opens on the first external backend, not db.
    const user = userEvent.setup();
    render(<SecretDialog slug="demo" configuredSources={["vault"]} />);
    const select = await openDialog();
    // Defaults to vault (the only backend) → external fields shown, no value.
    expect(screen.queryByLabelText("Value")).toBeNull();
    expect(screen.getByLabelText(/^Path/)).toBeTruthy();
    const listbox = await openSelect(user, select);
    const names = within(listbox)
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(names).toEqual(["HashiCorp Vault"]);
  });

  it("blocks submit when Vault has no key (no action dispatched)", async () => {
    const user = userEvent.setup();
    render(<SecretDialog slug="demo" configuredSources={["db", "vault"]} />);
    const select = await openDialog();
    await selectOption(user, select, "HashiCorp Vault");

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "GH_TOKEN" },
    });
    fireEvent.change(screen.getByLabelText(/^Path/), {
      target: { value: "secret/data/ci" },
    });
    // No key typed — submit must surface the error and NOT dispatch.
    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/key is required/i);
    expect(setSecret).not.toHaveBeenCalled();
  });
});

describe("SecretDialog dispatch", () => {
  it("forwards source + ref on a project external secret", async () => {
    const user = userEvent.setup();
    render(<SecretDialog slug="demo" configuredSources={["db", "vault"]} />);
    const select = await openDialog();
    await selectOption(user, select, "HashiCorp Vault");

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "GH_TOKEN" },
    });
    fireEvent.change(screen.getByLabelText(/^Path/), {
      target: { value: "secret/data/ci/github" },
    });
    fireEvent.change(screen.getByLabelText(/^Key/), {
      target: { value: "token" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => expect(setSecret).toHaveBeenCalledTimes(1));
    const arg = setSecret.mock.calls[0]![0];
    expect(arg.slug).toBe("demo");
    expect(arg.payload).toEqual({
      source: "vault",
      name: "GH_TOKEN",
      ref: { path: "secret/data/ci/github", key: "token" },
    });
  });

  it("forwards an inline value on a db secret", async () => {
    render(<SecretDialog slug="demo" configuredSources={[]} />);
    await openDialog();

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "API_KEY" },
    });
    fireEvent.change(screen.getByLabelText("Value"), {
      target: { value: "s3cr3t" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => expect(setSecret).toHaveBeenCalledTimes(1));
    expect(setSecret.mock.calls[0]![0].payload).toEqual({
      source: "db",
      name: "API_KEY",
      value: "s3cr3t",
    });
  });

  it("routes a global secret to setGlobalSecret with no slug", async () => {
    render(<SecretDialog scope="global" configuredSources={["aws"]} />);
    const select = await openDialog();
    // aws is the only backend → already the default; no need to reselect.
    void select;

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "SHARED" },
    });
    fireEvent.change(screen.getByLabelText(/^Path/), {
      target: { value: "ci/shared-token" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    await waitFor(() => expect(setGlobalSecret).toHaveBeenCalledTimes(1));
    // aws ref has no key → omitted; global payload has no slug wrapper.
    expect(setGlobalSecret.mock.calls[0]![0]).toEqual({
      source: "aws",
      name: "SHARED",
      ref: { path: "ci/shared-token" },
    });
    expect(setSecret).not.toHaveBeenCalled();
  });
});
