import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect } from "vitest";

import {
  collectNodeSelector,
  collectTolerations,
  SchedulingFields,
  type NodeSelectorRow,
  type TolerationRow,
} from "./scheduling-fields.client";
import { selectOption } from "@/test/select";

// Stateful harness — SchedulingFields is fully controlled, so the test
// owns the rows and lets the component drive them through setRows.
function Harness({ initial }: { initial: TolerationRow[] }) {
  const [tolerations, setTolerations] = useState<TolerationRow[]>(initial);
  const [nodeSelector, setNodeSelector] = useState<NodeSelectorRow[]>([]);
  return (
    <SchedulingFields
      nodeSelector={nodeSelector}
      setNodeSelector={setNodeSelector}
      tolerations={tolerations}
      setTolerations={setTolerations}
    />
  );
}

describe("TolerationEditorRow selects", () => {
  it("operator → Exists disables and clears the value field", async () => {
    const user = userEvent.setup();
    render(
      <Harness
        initial={[{ key: "spot", operator: "Equal", value: "true", effect: "" }]}
      />,
    );

    const value = screen.getByLabelText("Toleration value") as HTMLInputElement;
    expect(value.value).toBe("true");
    expect(value.disabled).toBe(false);

    await selectOption(user, screen.getByLabelText("Toleration operator"), "Exists");

    // Cross-field invariant: Exists forces an empty value and the input
    // is disabled (kubelet rejects a value with operator=Exists).
    expect(value.disabled).toBe(true);
    expect(value.value).toBe("");
  });

  it("effect → NoExecute enables the toleration_seconds field", async () => {
    const user = userEvent.setup();
    render(
      <Harness
        initial={[{ key: "spot", operator: "Exists", value: "", effect: "" }]}
      />,
    );

    const seconds = screen.getByLabelText(
      /Toleration seconds/,
    ) as HTMLInputElement;
    expect(seconds.disabled).toBe(true);

    await selectOption(user, screen.getByLabelText("Toleration effect"), "NoExecute");
    expect(seconds.disabled).toBe(false);
  });
});

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
  it("drops ONLY rows still on the default ('+Add' clicked but nothing typed)", () => {
    // A row whose key is empty but ANY other field was touched
    // (operator switched to Exists, a value typed, an effect
    // picked, seconds entered) must reach the server so the
    // canonical error message comes back instead of the row
    // vanishing without explanation.
    const rows: TolerationRow[] = [
      // Pure default — silently dropped.
      { key: "", operator: "Equal", value: "", effect: "" },
      // Exists+empty-key — kubelet "tolerate everything" pattern, kept.
      { key: "", operator: "Exists", value: "", effect: "" },
      // Equal+empty-key but value typed — kept; server returns
      // "key required unless operator=Exists" (loud).
      { key: "", operator: "Equal", value: "true", effect: "" },
      // Equal+empty-key but effect picked — kept; server complains
      // server-side rather than the UI swallowing it.
      { key: "", operator: "Equal", value: "", effect: "NoSchedule" },
      // Equal+empty-key but seconds entered — kept; server rejects.
      {
        key: "",
        operator: "Equal",
        value: "",
        effect: "",
        toleration_seconds: 60,
      },
      // Fully filled row — kept.
      { key: "ci-only", operator: "Equal", value: "true", effect: "NoSchedule" },
    ];
    const got = collectTolerations(rows);
    expect(got).toHaveLength(5);
    expect(got[0]).toMatchObject({ key: "", operator: "Exists" });
    expect(got[1]).toMatchObject({ key: "", value: "true" });
    expect(got[2]).toMatchObject({ key: "", effect: "NoSchedule" });
    expect(got[3]).toMatchObject({ key: "", toleration_seconds: 60 });
    expect(got[4]).toMatchObject({
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
