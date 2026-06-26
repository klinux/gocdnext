import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { ServicesCluster } from "@/components/pipelines/services-cluster";

describe("ServicesCluster", () => {
  it("renders nothing when the run declares no services", () => {
    const { container } = render(<ServicesCluster names={[]} tone="success" />);
    expect(container.textContent).toBe("");
  });

  it("shows the services label and one tile per declared service", () => {
    render(<ServicesCluster names={["postgres", "redis"]} tone="success" />);
    expect(screen.getByText("services")).toBeTruthy();
    // one tinted glyph (svg) per tile
    expect(document.querySelectorAll("svg")).toHaveLength(2);
  });

  it("collapses services beyond the tile cap into a +N counter", () => {
    render(
      <ServicesCluster
        names={["postgres", "redis", "mongo", "kafka", "nats"]}
        tone="running"
      />,
    );
    // MAX_TILES = 3 → three tiles rendered, the remaining two collapse to +2
    expect(document.querySelectorAll("svg")).toHaveLength(3);
    expect(screen.getByText("+2")).toBeTruthy();
  });
});
