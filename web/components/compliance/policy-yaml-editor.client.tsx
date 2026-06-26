"use client";

// EditorView comes from @uiw/react-codemirror's re-export (export * from
// @codemirror/view) so we bind the SAME copy it bundles — importing
// @codemirror/view directly risks a duplicate that CodeMirror rejects at
// runtime ("Unrecognized extension value").
import CodeMirror, { EditorView } from "@uiw/react-codemirror";
import { yamlSchema } from "codemirror-json-schema/yaml";
import { useMemo } from "react";

import policyFragmentSchema from "@/lib/schema/policy-fragment.schema.json";

type Props = {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
};

// PolicyYamlEditor is the schema-aware CodeMirror editor for a compliance
// policy body. It feeds the generated policy-fragment schema (the exact subset
// CompilePolicy keeps — stages + _compliance_-prefixed jobs) to the YAML
// language server bundle, so the admin gets key autocomplete, hover docs, and
// inline validation (including the reserved-prefix rule) instead of a raw
// textarea that only errors on submit.
//
// Loaded via next/dynamic (ssr: false) from the policy form — CodeMirror is
// browser-only and heavy, so it stays off the server bundle and the initial
// client chunk.
export default function PolicyYamlEditor({ id, value, onChange, placeholder }: Props) {
  // The bundle wires the YAML language, schema linter, completion source, and
  // hover tooltip. We also attach the label to the editable content (not the
  // wrapper div, where CodeMirror would otherwise put the id) so the form's
  // <label> announces and focuses the actual textbox.
  const extensions = useMemo(() => {
    const attrs: Record<string, string> = { "aria-label": "Policy config (YAML)" };
    if (id) {
      attrs.id = id;
    }
    return [
      ...yamlSchema(policyFragmentSchema as Parameters<typeof yamlSchema>[0]),
      EditorView.contentAttributes.of(attrs),
    ];
  }, [id]);

  return (
    <CodeMirror
      value={value}
      onChange={onChange}
      placeholder={placeholder}
      extensions={extensions}
      theme="dark"
      basicSetup={{ foldGutter: false, highlightActiveLine: false }}
      minHeight="16rem"
      maxHeight="32rem"
      className="overflow-hidden rounded-md border border-input font-mono text-xs"
    />
  );
}
