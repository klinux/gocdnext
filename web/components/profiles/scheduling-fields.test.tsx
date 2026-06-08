import { describe, it, expect } from "vitest";

import {
  collectNodeSelector,
  collectTolerations,
  type NodeSelectorRow,
  type TolerationRow,
} from "./scheduling-fields.client";

describe("collectNodeSelector", () => {
  it("drops empty-key rows and trims keys", () => {
    const rows: NodeSelectorRow[] = [
      { key: "  workload  ", value: "ci" },
      { key: "", value: "ignored" },
      { key: "pool", value: "gradle" },
    ];
    expect(collectNodeSelector(rows)).toEqual({
      workload: "ci",
      pool: "gradle",
    });
  });

  it("preserves empty values when the key is present", () => {
    // An empty label value is legal at the apiserver level; the
    // collector must not drop the row just because value is blank.
    expect(collectNodeSelector([{ key: "ci-only", value: "" }])).toEqual({
      "ci-only": "",
    });
  });
});

describe("collectTolerations", () => {
  it("drops Equal rows with empty key but keeps Exists with empty key", () => {
    // Equal+empty-key is meaningless (server rejects). The kubelet
    // "tolerate everything" pattern is Exists+empty-key — keep it.
    const rows: TolerationRow[] = [
      { key: "", operator: "Equal", value: "", effect: "" },
      { key: "", operator: "Exists", value: "", effect: "" },
      { key: "ci-only", operator: "Equal", value: "true", effect: "NoSchedule" },
    ];
    const got = collectTolerations(rows);
    expect(got).toHaveLength(2);
    expect(got[0]).toMatchObject({ key: "", operator: "Exists" });
    expect(got[1]).toMatchObject({
      key: "ci-only",
      operator: "Equal",
      value: "true",
      effect: "NoSchedule",
    });
  });

  it("normalises toleration_seconds to null when unset", () => {
    const rows: TolerationRow[] = [
      { key: "spot", operator: "Exists", value: "", effect: "NoExecute" },
    ];
    const got = collectTolerations(rows);
    expect(got[0]?.toleration_seconds).toBeNull();
  });

  it("forwards toleration_seconds when set", () => {
    const rows: TolerationRow[] = [
      {
        key: "spot",
        operator: "Exists",
        value: "",
        effect: "NoExecute",
        toleration_seconds: 60,
      },
    ];
    const got = collectTolerations(rows);
    expect(got[0]?.toleration_seconds).toBe(60);
  });
});
