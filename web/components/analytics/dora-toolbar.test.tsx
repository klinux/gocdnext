import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { selectOption } from "@/test/select";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

// Imported after the mock so the component picks up the stubbed router.
import { DoraToolbar } from "./dora-toolbar.client";

function renderToolbar(activeEnv: string) {
  render(
    <DoraToolbar
      keys={["team"]}
      activeKey="team"
      windowDays={30}
      environments={["all", "prod"]}
      activeEnv={activeEnv}
    />,
  );
}

describe("DoraToolbar environment filter", () => {
  beforeEach(() => push.mockClear());

  it("filters by a real environment named 'all' (no sentinel collision)", async () => {
    const user = userEvent.setup();
    renderToolbar("");
    await selectOption(user, screen.getByLabelText("Environment"), "all");
    expect(push).toHaveBeenCalledTimes(1);
    expect(push.mock.calls[0]![0]).toContain("env=all");
  });

  it("drops the env param when choosing 'All environments'", async () => {
    const user = userEvent.setup();
    renderToolbar("all");
    await selectOption(user, screen.getByLabelText("Environment"), "All environments");
    expect(push).toHaveBeenCalledTimes(1);
    expect(push.mock.calls[0]![0]).not.toContain("env=");
  });
});
