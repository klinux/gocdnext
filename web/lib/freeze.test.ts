import { describe, expect, it } from "vitest";

import { jobDisplayStatus, stageDisplayStatus } from "./freeze";
import type { JobDetail, StageDetail } from "@/types/api";

function job(overrides: Partial<JobDetail> = {}): JobDetail {
  return {
    id: "j1",
    stage_run_id: "s1",
    name: "approve",
    status: "awaiting_approval",
    attempt: 0,
    approval_gate: true,
    ...overrides,
  };
}

function stage(overrides: Partial<StageDetail> = {}): StageDetail {
  return {
    id: "s1",
    name: "deploy",
    ordinal: 0,
    status: "running",
    jobs: [job()],
    ...overrides,
  };
}

describe("stageDisplayStatus", () => {
  it("presents a running stage as freezing while its approval gate is freeze-held", () => {
    expect(
      stageDisplayStatus(
        stage({ jobs: [job({ held_by_freeze: true, frozen_envs: ["prod"] })] }),
      ),
    ).toBe("freezing");
  });

  it("keeps the persisted status when the gate is awaiting ordinary approval", () => {
    expect(stageDisplayStatus(stage())).toBe("running");
  });

  it("does not resurrect a terminal stage from stale freeze metadata", () => {
    expect(
      stageDisplayStatus(
        stage({
          status: "success",
          jobs: [job({ status: "success", held_by_freeze: true })],
        }),
      ),
    ).toBe("success");
  });
});

describe("jobDisplayStatus", () => {
  it("derives freezing only from a held approval gate", () => {
    expect(jobDisplayStatus(job({ held_by_freeze: true }))).toBe("freezing");
    expect(jobDisplayStatus(job({ approval_gate: false, held_by_freeze: true }))).toBe(
      "awaiting_approval",
    );
  });
});
