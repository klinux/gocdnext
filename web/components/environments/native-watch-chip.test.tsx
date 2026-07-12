import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { NativeWatchChip } from "./native-watch-chip.client";
import type { DeployWatch } from "@/types/api";

const base: DeployWatch = {
  environment: "production",
  version: "1.4.2",
  expected_revision: "abc0123456789def",
  watch_started_at: "2026-07-12T10:00:00Z",
  deadline_at: "2026-07-12T10:15:00Z",
};

describe("NativeWatchChip", () => {
  it("shows Syncing with a truncated revision once the sync has fired", () => {
    render(
      <NativeWatchChip watch={{ ...base, sync_requested_at: "2026-07-12T10:00:05Z" }} />,
    );
    expect(screen.getByText("Syncing")).toBeTruthy();
    expect(screen.getByText("abc0123")).toBeTruthy(); // 7-char short SHA
  });

  it("reads Deploying before the sync fires (no sync_requested_at)", () => {
    render(<NativeWatchChip watch={base} />);
    expect(screen.getByText("Deploying")).toBeTruthy();
  });

  it("shows Degraded (and it wins over Syncing) when the rollout is degraded", () => {
    render(
      <NativeWatchChip
        watch={{
          ...base,
          sync_requested_at: "2026-07-12T10:00:05Z",
          degraded_since: "2026-07-12T10:01:00Z",
        }}
      />,
    );
    expect(screen.getByText("Degraded")).toBeTruthy();
    expect(screen.queryByText("Syncing")).toBeNull();
  });
});
