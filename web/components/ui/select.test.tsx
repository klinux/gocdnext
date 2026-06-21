import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./select";
import { selectOption } from "@/test/select";

const LABELS: Record<string, string> = {
  a: "Apple",
  b: "Banana",
  c: "Cherry",
};

function Fixture({
  value,
  onValueChange,
  disabled,
}: {
  value?: string;
  onValueChange?: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <Select
      items={LABELS}
      defaultValue={value}
      onValueChange={(v) => v && onValueChange?.(v)}
      disabled={disabled}
    >
      <SelectTrigger aria-label="Fruit">
        <SelectValue placeholder="Pick one" />
      </SelectTrigger>
      <SelectContent>
        {Object.entries(LABELS).map(([v, label]) => (
          <SelectItem key={v} value={v}>
            {label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

describe("Select (shadcn / base-ui)", () => {
  it("shows the selected item's label, not the raw value", () => {
    render(<Fixture value="b" />);
    // Regression guard: base-ui's Select.Value renders the raw value
    // unless `items` maps it to a label. The trigger must read "Banana".
    expect(screen.getByLabelText("Fruit").textContent).toContain("Banana");
  });

  it("renders the placeholder when nothing is selected", () => {
    render(<Fixture />);
    expect(screen.getByLabelText("Fruit").textContent).toContain("Pick one");
  });

  it("opens and reports the picked value", async () => {
    const user = userEvent.setup();
    const onValueChange = vi.fn();
    render(<Fixture value="a" onValueChange={onValueChange} />);

    await selectOption(user, screen.getByLabelText("Fruit"), "Cherry");

    await waitFor(() => expect(onValueChange).toHaveBeenCalledWith("c"));
    expect(screen.getByLabelText("Fruit").textContent).toContain("Cherry");
  });

  it("does not open when disabled", async () => {
    const user = userEvent.setup();
    render(<Fixture value="a" disabled />);

    await user.click(screen.getByLabelText("Fruit"));
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});
