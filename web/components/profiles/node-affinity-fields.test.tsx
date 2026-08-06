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
    expect(rows[0]?.match_expressions[1]?.values).toEqual([
      "us-central1-a",
      "us-central1-b",
    ]);
    expect(collectPreferredNodeAffinity(rows)).toEqual(api);
  });

  it('round-trips an empty label value [""] without dropping it', () => {
    // k8s accepts In/NotIn with values: [""]; the editor must not collapse it.
    const api: AdminPreferredNodeAffinityTerm[] = [
      { weight: 10, match_expressions: [{ key: "k", operator: "In", values: [""] }] },
    ];
    const rows = affinityTermsFromApi(api);
    expect(rows[0]?.match_expressions[0]?.values).toEqual([""]);
    expect(collectPreferredNodeAffinity(rows)).toEqual(api);
  });

  it("keeps values [] when none are provided (distinct from an explicit [\"\"])", () => {
    // A key-only In expression must NOT silently become [""] — it stays [] so
    // the server rejects it loudly ("In needs a value").
    const rows: AffinityTermRow[] = [
      { weight: 100, match_expressions: [{ key: "k", operator: "In", values: [] }] },
    ];
    expect(
      collectPreferredNodeAffinity(rows)[0]?.match_expressions[0]?.values,
    ).toEqual([]);
  });

  it("forces [] for Exists and drops keyless expressions / empty terms", () => {
    const rows: AffinityTermRow[] = [
      { weight: 100, match_expressions: [{ key: "", operator: "In", values: ["x"] }] },
      {
        weight: 50,
        match_expressions: [{ key: "k", operator: "Exists", values: ["ignored"] }],
      },
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

  it("a fresh expression has no value input until '+ value' is clicked", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: /add affinity term/i }));
    expect(screen.queryAllByLabelText("Affinity value")).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: /\+ value/i }));
    expect(screen.getAllByLabelText("Affinity value")).toHaveLength(1);
  });
});
