import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { ProjectSettingsTabs } from "./project-settings-tabs.client";

// A stand-in for a settings card that holds local state (like Labels, Polling or
// Compliance), used to prove an unsaved edit survives a tab switch.
function StatefulField() {
  const [value, setValue] = useState("");
  return (
    <input
      aria-label="draft"
      value={value}
      onChange={(e) => setValue(e.target.value)}
    />
  );
}

describe("ProjectSettingsTabs", () => {
  it("shows the first tab by default and switches panels on click", () => {
    render(
      <ProjectSettingsTabs
        tabs={[
          { value: "general", label: "General", content: <p>panel one</p> },
          {
            value: "checks",
            label: "GitHub checks",
            content: <p>panel two</p>,
          },
        ]}
      />,
    );

    const generalTab = screen.getByRole("tab", { name: "General" });
    const checksTab = screen.getByRole("tab", { name: "GitHub checks" });

    // First tab active by default; its panel is rendered.
    expect(generalTab.getAttribute("aria-selected")).toBe("true");
    expect(checksTab.getAttribute("aria-selected")).toBe("false");
    expect(screen.getByText("panel one")).toBeTruthy();

    // Clicking the second tab makes it active and reveals its panel.
    fireEvent.click(checksTab);
    expect(checksTab.getAttribute("aria-selected")).toBe("true");
    expect(generalTab.getAttribute("aria-selected")).toBe("false");
    expect(screen.getByText("panel two")).toBeTruthy();
  });

  it("preserves a tab's unsaved state across a switch away and back", () => {
    render(
      <ProjectSettingsTabs
        tabs={[
          { value: "a", label: "Tab A", content: <StatefulField /> },
          { value: "b", label: "Tab B", content: <p>panel b</p> },
        ]}
      />,
    );

    const field = screen.getByLabelText("draft") as HTMLInputElement;
    fireEvent.change(field, { target: { value: "unsaved edit" } });
    expect(field.value).toBe("unsaved edit");

    // Switch to Tab B and back to Tab A: the field must keep its value (the
    // panel stays mounted via keepMounted, so no remount wipes the edit).
    fireEvent.click(screen.getByRole("tab", { name: "Tab B" }));
    fireEvent.click(screen.getByRole("tab", { name: "Tab A" }));
    expect((screen.getByLabelText("draft") as HTMLInputElement).value).toBe(
      "unsaved edit",
    );
  });

  it("renders nothing when there are no tabs", () => {
    const { container } = render(<ProjectSettingsTabs tabs={[]} />);
    expect(container.firstChild).toBeNull();
  });
});
