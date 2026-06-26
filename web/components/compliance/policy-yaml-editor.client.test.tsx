import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

// CodeMirror is a heavy browser widget; stub it with a textarea so we test the
// wrapper's contract (value in → string out) deterministically in jsdom. The
// real editor + schema enforcement is exercised by the production build and the
// Go schema tests (server/cmd/schemagen).
// codemirror-json-schema/yaml uses extensionless internal ESM imports Vitest's
// resolver can't follow; stub it (its real behaviour is the libs' concern).
vi.mock("codemirror-json-schema/yaml", () => ({ yamlSchema: () => [] }));

vi.mock("@uiw/react-codemirror", () => ({
  default: ({
    value,
    onChange,
  }: {
    value: string;
    onChange: (v: string) => void;
  }) => (
    <textarea
      aria-label="policy yaml"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  ),
  // The component pulls EditorView from this re-export; stub the one member it
  // uses (contentAttributes.of) so the extension array builds without CM.
  EditorView: { contentAttributes: { of: () => ({}) } },
}));

import PolicyYamlEditor from "./policy-yaml-editor.client";

describe("PolicyYamlEditor", () => {
  it("renders the current value", () => {
    render(<PolicyYamlEditor value="stages: [_compliance_x]" onChange={() => {}} />);
    const ta = screen.getByLabelText("policy yaml") as HTMLTextAreaElement;
    expect(ta.value).toBe("stages: [_compliance_x]");
  });

  it("emits edited yaml as a string (not a DOM event)", async () => {
    const onChange = vi.fn();
    render(<PolicyYamlEditor value="" onChange={onChange} />);
    await userEvent.type(screen.getByLabelText("policy yaml"), "x");
    expect(onChange).toHaveBeenCalledWith("x");
  });
});
