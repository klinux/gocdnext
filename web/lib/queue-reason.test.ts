import { describe, expect, it } from "vitest";

import { formatQueueReason } from "./queue-reason";

describe("formatQueueReason", () => {
  it("labels a frozen deploy with its environment", () => {
    const got = formatQueueReason("frozen-deploy:production");
    expect(got?.label).toBe("Frozen: production");
  });

  it("says the freeze holds the deploy, not the whole run", () => {
    // A stage can hold a frozen `production` deploy while its `staging` sibling
    // dispatches. If the tooltip read as "the run is halted", an operator would
    // go looking for a problem that isn't there.
    const got = formatQueueReason("frozen-deploy:production");
    expect(got?.title).toMatch(/other jobs in the run are unaffected/i);
  });

  it("keeps an environment name containing an underscore intact", () => {
    // `_` is legal in an environment name and is a LIKE wildcard server-side —
    // the reason is split on the FIRST colon only, so the detail is verbatim.
    expect(formatQueueReason("frozen-deploy:staging_eu")?.label).toBe(
      "Frozen: staging_eu",
    );
  });

  it("returns null for absent, malformed or detail-less reasons", () => {
    expect(formatQueueReason(undefined)).toBeNull();
    expect(formatQueueReason("")).toBeNull();
    expect(formatQueueReason("frozen-deploy")).toBeNull();
    expect(formatQueueReason("frozen-deploy:")).toBeNull();
    expect(formatQueueReason(":production")).toBeNull();
  });

  it("returns null for an unknown key rather than echoing an internal token", () => {
    // A future server-side producer must not leak `some-new-gate:9f3e…` into
    // the UI: no pill reads better than a meaningless one.
    expect(formatQueueReason("serial-busy:9f3e0c2a")).toBeNull();
    expect(formatQueueReason("some-new-gate:whatever")).toBeNull();
  });
});
