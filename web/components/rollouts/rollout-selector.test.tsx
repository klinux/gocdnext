import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

import { RolloutSelector } from "./rollout-selector.client";

describe("RolloutSelector (needs-params state)", () => {
  beforeEach(() => push.mockClear());

  it("asks for a cluster and namespace with labelled inputs and a Load button", () => {
    render(<RolloutSelector basePath="/projects/acme/rollouts" />);
    expect(screen.getByLabelText("Cluster")).toBeTruthy();
    expect(screen.getByLabelText("Namespace")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Load" })).toBeTruthy();
    expect(
      screen.getByText(/Rollouts are read per Kubernetes cluster and namespace/),
    ).toBeTruthy();
  });

  it("disables Load until both a cluster and a namespace are provided", () => {
    render(<RolloutSelector basePath="/projects/acme/rollouts" />);
    const load = screen.getByRole("button", { name: "Load" }) as HTMLButtonElement;
    expect(load.disabled).toBe(true);
  });

  it("navigates to the same route with cluster + namespace query params on Load", () => {
    render(
      <RolloutSelector
        basePath="/projects/acme/rollouts"
        defaultCluster="prod-hub"
        defaultNamespace="production"
      />,
    );
    const load = screen.getByRole("button", { name: "Load" }) as HTMLButtonElement;
    expect(load.disabled).toBe(false);
    fireEvent.click(load);
    expect(push).toHaveBeenCalledWith(
      "/projects/acme/rollouts?cluster=prod-hub&namespace=production",
    );
  });

  it("renders configured targets as one-click picks and navigates on click", () => {
    render(
      <RolloutSelector
        basePath="/projects/acme/rollouts"
        picks={[
          {
            environment: "prod",
            rolloutName: "smoke-canary",
            cluster: "lab-inc",
            namespace: "smoke-rollout",
          },
        ]}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /prod/ }));
    expect(push).toHaveBeenCalledWith(
      "/projects/acme/rollouts?cluster=lab-inc&namespace=smoke-rollout",
    );
  });

  it("collapses manual entry when picks exist and reveals it on toggle", () => {
    render(
      <RolloutSelector
        basePath="/projects/acme/rollouts"
        picks={[
          { environment: "prod", cluster: "lab-inc", namespace: "smoke-rollout" },
        ]}
      />,
    );
    expect(screen.queryByLabelText("Cluster")).toBeNull();
    fireEvent.click(
      screen.getByRole("button", {
        name: /enter a cluster and namespace manually/i,
      }),
    );
    expect(screen.getByLabelText("Cluster")).toBeTruthy();
    expect(screen.getByLabelText("Namespace")).toBeTruthy();
  });
});
