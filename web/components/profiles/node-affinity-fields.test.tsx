import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import type { AdminPreferredNodeAffinityTerm } from "@/server/queries/admin";

import {
  type AffinityTermRow,
  affinityTermsFromApi,
  collectPreferredNodeAffinity,
  PreferredNodeAffinityEditor,
} from "./node-affinity-fields.client";

describe("preferred node affinity collect/hydrate", () => {
  it("round-trips MULTIPLE expressions per term without dropping any", () => {
    const api: AdminPreferredNodeAffinityTerm[] = [
      {
        weight: 80,
        match_expressions: [
          { key: "cloud.google.com/gke-spot", operator: "In", values: ["true"] },
          {
            key: "topology.kubernetes.io/zone",
            operator: "In",
            values: ["us-central1-a", "us-central1-b"],
          },
        ],
      },
    ];
    const rows = affinityTermsFromApi(api);
    expect(rows[0]?.match_expressions).toHaveLength(2);
    expect(rows[0]?.match_expressions[1]?.values).toBe(
      "us-central1-a, us-central1-b",
    );
    // Save transform must reproduce the same two expressions — the editing
    // round-trip never collapses a multi-expression term to its first entry.
    expect(collectPreferredNodeAffinity(rows)).toEqual(api);
  });

  it("drops expressions with no key and terms left without any expression", () => {
    const rows: AffinityTermRow[] = [
      { weight: 100, match_expressions: [{ key: "", operator: "In", values: "x" }] },
      { weight: 50, match_expressions: [{ key: "k", operator: "Exists", values: "" }] },
    ];
    const out = collectPreferredNodeAffinity(rows);
    expect(out).toHaveLength(1);
    expect(out[0]?.weight).toBe(50);
    expect(out[0]?.match_expressions[0]?.values).toEqual([]);
  });
});

describe("PreferredNodeAffinityEditor", () => {
  function Harness() {
    const [rows, setRows] = useState<AffinityTermRow[]>([]);
    return <PreferredNodeAffinityEditor rows={rows} setRows={setRows} />;
  }

  it("supports adding a second expression within a term (nested editor)", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: /add affinity term/i }));
    expect(screen.getAllByLabelText("Affinity key")).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: /add expression/i }));
    expect(screen.getAllByLabelText("Affinity key")).toHaveLength(2);
  });
});
