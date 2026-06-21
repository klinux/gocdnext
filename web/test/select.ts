import { screen } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

// Test helpers for the shadcn Select (built on @base-ui/react/select).
//
// A Base UI Select is NOT a native <select>: there are no <option>
// nodes to fire a `change` event at. The trigger is a button and the
// list renders in a portal once opened. fireEvent does not reliably
// drive its pointer-based selection in jsdom — user-event does. These
// helpers wrap the open → click-option dance so component tests read
// the same regardless of which select they touch.

// openSelect clicks the trigger and resolves once the listbox is on
// screen. Returns the listbox so callers can scope option queries when
// several selects share option labels.
export async function openSelect(
  user: UserEvent,
  trigger: HTMLElement,
): Promise<HTMLElement> {
  await user.click(trigger);
  return screen.findByRole("listbox");
}

// selectOption opens the select at `trigger` and picks the option whose
// accessible name matches `option`. Awaiting it leaves the popup closed
// and the value committed.
export async function selectOption(
  user: UserEvent,
  trigger: HTMLElement,
  option: string | RegExp,
): Promise<void> {
  await user.click(trigger);
  const item = await screen.findByRole("option", { name: option });
  await user.click(item);
}
