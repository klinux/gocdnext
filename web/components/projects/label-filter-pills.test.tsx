import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { LabelFilterPills, type LabelChip } from "./label-filter-pills.client";

function chip(key: string, value: string, count = 1): LabelChip {
  return { id: JSON.stringify([key, value]), key, value, count };
}

describe("LabelFilterPills", () => {
  it("renders every label inline when they fit under the cap", () => {
    const labels = [chip("team", "card"), chip("team", "internal"), chip("env", "prod")];
    render(<LabelFilterPills labels={labels} selected={null} onSelect={vi.fn()} />);

    expect(screen.getByRole("button", { name: /team:card/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /env:prod/i })).toBeTruthy();
    // No overflow menu when everything fits.
    expect(
      screen.queryByRole("button", { name: /more label filters/i }),
    ).toBeNull();
  });

  it("collapses the overflow into a +N menu, hiding the extra labels", () => {
    const labels = ["a", "b", "c", "d", "e", "f"].map((v) => chip("team", v));
    render(
      <LabelFilterPills labels={labels} selected={null} onSelect={vi.fn()} maxInline={4} />,
    );

    // First four inline.
    expect(screen.getByRole("button", { name: /team:a/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /team:d/i })).toBeTruthy();
    // The trigger reports the two hidden ones.
    const trigger = screen.getByRole("button", { name: /show 2 more label filters/i });
    expect(trigger.textContent).toContain("+2");
    // e and f are not on screen until the menu opens.
    expect(screen.queryByRole("button", { name: /team:e/i })).toBeNull();
    expect(screen.queryByRole("option", { name: /team:f/i })).toBeNull();
  });

  it("keeps the active label inline even when it belongs to the overflow", () => {
    const labels = ["a", "b", "c", "d", "e", "f"].map((v) => chip("team", v));
    render(
      <LabelFilterPills
        labels={labels}
        selected={JSON.stringify(["team", "f"])}
        onSelect={vi.fn()}
        maxInline={4}
      />,
    );

    // The selected label (which sorts into the overflow) is surfaced inline
    // and marked active, so the current selection never hides behind the menu.
    const active = screen.getByRole("button", { name: /team:f/i });
    expect(active.getAttribute("aria-pressed")).toBe("true");
    // inline = [f, a, b, c] → overflow = d, e.
    expect(
      screen.getByRole("button", { name: /show 2 more label filters/i }),
    ).toBeTruthy();
  });

  it("opens the menu, filters by the search box, and selects a label", () => {
    const onSelect = vi.fn();
    const labels = [
      chip("team", "alpha"),
      chip("team", "beta"),
      chip("env", "prod"),
      chip("env", "stage"),
      chip("region", "us"),
      chip("region", "eu"),
    ];
    render(
      <LabelFilterPills labels={labels} selected={null} onSelect={onSelect} maxInline={4} />,
    );

    fireEvent.click(screen.getByRole("button", { name: /more label filters/i }));

    const search = screen.getByRole("textbox", { name: /search labels/i });
    fireEvent.change(search, { target: { value: "region" } });

    // Only region:* survive the filter; a non-matching label is gone.
    expect(screen.getByRole("option", { name: /region:us/i })).toBeTruthy();
    expect(screen.queryByRole("option", { name: /team:alpha/i })).toBeNull();

    fireEvent.click(screen.getByRole("option", { name: /region:eu/i }));
    expect(onSelect).toHaveBeenCalledWith(JSON.stringify(["region", "eu"]));
  });
});
