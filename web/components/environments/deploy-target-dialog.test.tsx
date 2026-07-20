import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { DeployTargetDialog } from "./deploy-target-dialog.client";
import type { DeployTarget } from "@/types/api";

const setDeployTarget = vi.fn(async (_input: unknown) => ({ ok: true as const }));
const deleteDeployTarget = vi.fn(async (_input: unknown) => ({
  ok: true as const,
}));

vi.mock("@/server/actions/environments", () => ({
  setDeployTarget: (input: unknown) => setDeployTarget(input),
  deleteDeployTarget: (input: unknown) => deleteDeployTarget(input),
}));
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn() }),
}));

const target: DeployTarget = {
  environment: "production",
  provider: "argocd",
  cluster: "prod-hub",
  application: "checkout",
  namespace: "argocd",
  sync_mode: "trigger",
};

afterEach(() => {
  vi.clearAllMocks();
});

// The dialog mounts its content in a portal once opened.
function open(name: RegExp) {
  fireEvent.click(screen.getByRole("button", { name }));
}

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
}

describe("DeployTargetDialog", () => {
  it("registers a new target with the entered fields", async () => {
    render(
      <DeployTargetDialog
        slug="acme"
        trigger={<button type="button">Register native target</button>}
      />,
    );
    open(/register native target/i);

    fireEvent.change(await screen.findByLabelText("Environment"), {
      target: { value: "staging" },
    });
    fireEvent.change(screen.getByLabelText(/Cluster/), {
      target: { value: "argocd-hub" },
    });
    fireEvent.change(screen.getByLabelText(/ArgoCD Application/), {
      target: { value: "shop-staging" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^Register$/ }));
    await flush();

    expect(setDeployTarget).toHaveBeenCalledTimes(1);
    expect(setDeployTarget.mock.calls[0]?.[0]).toMatchObject({
      slug: "acme",
      environment: "staging",
      cluster: "argocd-hub",
      application: "shop-staging",
      sync_mode: "trigger",
    });
    // Rollout observation off by default.
    expect(setDeployTarget.mock.calls[0]?.[0]).toMatchObject({ rollout_aware: false });
  });

  it("round-trips governing_gate unchanged when editing a gated target", async () => {
    // A maintainer editing a non-gate field on a gated target must send the gate back
    // verbatim — the server reads an absent gate as "remove it" (admin-only) → 403.
    const gated: DeployTarget = {
      ...target,
      rollout_aware: true,
      governing_gate: { required: 2, approvers: ["alice@example.com"] },
    };
    render(
      <DeployTargetDialog slug="acme" initial={gated} trigger={<button type="button">Edit</button>} />,
    );
    open(/edit/i);
    fireEvent.change(await screen.findByLabelText(/ArgoCD Application/), {
      target: { value: "checkout-v2" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Save$/ }));
    await flush();

    expect(setDeployTarget.mock.calls[0]?.[0]).toMatchObject({
      application: "checkout-v2",
      governing_gate: { required: 2, approvers: ["alice@example.com"] },
    });
  });

  it("sends rollout_aware + routing when the rollout toggle is on", async () => {
    render(
      <DeployTargetDialog
        slug="acme"
        trigger={<button type="button">Register native target</button>}
      />,
    );
    open(/register native target/i);
    fireEvent.change(await screen.findByLabelText("Environment"), {
      target: { value: "prod" },
    });
    fireEvent.change(screen.getByLabelText(/Cluster/), { target: { value: "hub" } });
    fireEvent.change(screen.getByLabelText(/ArgoCD Application/), {
      target: { value: "shop" },
    });
    // Toggle rollout observation on → the routing inputs appear.
    fireEvent.click(screen.getByLabelText("Observe Argo Rollouts progress"));
    fireEvent.change(await screen.findByLabelText("Rollout name"), {
      target: { value: "shop-ro" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^Register$/ }));
    await flush();

    expect(setDeployTarget.mock.calls[0]?.[0]).toMatchObject({
      rollout_aware: true,
      rollout_name: "shop-ro",
    });
  });

  it("clears the fields after a successful register (no stale values on reopen)", async () => {
    render(
      <DeployTargetDialog
        slug="acme"
        trigger={<button type="button">Register native target</button>}
      />,
    );
    open(/register native target/i);
    fireEvent.change(await screen.findByLabelText("Environment"), {
      target: { value: "staging" },
    });
    fireEvent.change(screen.getByLabelText(/Cluster/), {
      target: { value: "hub" },
    });
    fireEvent.change(screen.getByLabelText(/ArgoCD Application/), {
      target: { value: "app" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Register$/ }));
    await waitFor(() => expect(setDeployTarget).toHaveBeenCalled());

    // Reopen — the environment field must be back to empty, not "staging".
    open(/register native target/i);
    const env = (await screen.findByLabelText("Environment")) as HTMLInputElement;
    expect(env.value).toBe("");
  });

  it("rejects an invalid environment name without dispatching", async () => {
    const { toast } = await import("sonner");
    render(
      <DeployTargetDialog
        slug="acme"
        trigger={<button type="button">Register native target</button>}
      />,
    );
    open(/register native target/i);

    fireEvent.change(await screen.findByLabelText("Environment"), {
      target: { value: "-nope" },
    });
    fireEvent.change(screen.getByLabelText(/Cluster/), {
      target: { value: "hub" },
    });
    fireEvent.change(screen.getByLabelText(/ArgoCD Application/), {
      target: { value: "app" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Register$/ }));
    await flush();

    expect(setDeployTarget).not.toHaveBeenCalled();
    expect(toast.error).toHaveBeenCalled();
  });

  it("locks the environment and pre-fills fields in edit mode", async () => {
    render(
      <DeployTargetDialog
        slug="acme"
        initial={target}
        trigger={<button type="button">Edit target</button>}
      />,
    );
    open(/edit target/i);

    const envInput = (await screen.findByLabelText(
      "Environment",
    )) as HTMLInputElement;
    expect(envInput.value).toBe("production");
    expect(envInput.disabled).toBe(true);

    fireEvent.change(screen.getByLabelText(/Cluster/), {
      target: { value: "prod-hub-2" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Save$/ }));
    await flush();

    expect(setDeployTarget.mock.calls[0]?.[0]).toMatchObject({
      slug: "acme",
      environment: "production",
      cluster: "prod-hub-2",
      application: "checkout",
    });
  });

  it("removes a target after confirmation", async () => {
    render(
      <DeployTargetDialog
        slug="acme"
        initial={target}
        trigger={<button type="button">Edit target</button>}
      />,
    );
    open(/edit target/i);
    await screen.findByLabelText("Environment");

    fireEvent.click(screen.getByRole("button", { name: /^Remove$/ }));
    // The destructive action is behind a confirm step.
    fireEvent.click(screen.getByRole("button", { name: /^Confirm$/ }));
    await flush();

    await waitFor(() =>
      expect(deleteDeployTarget).toHaveBeenCalledWith({
        slug: "acme",
        environment: "production",
      }),
    );
    expect(setDeployTarget).not.toHaveBeenCalled();
  });
});
