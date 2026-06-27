"use client";

import { useEffect, useState } from "react";

import { previewDraftPolicy } from "@/server/actions/compliance";
import type { EffectivePipelinePreview } from "@/server/queries/admin";
import type { PolicyDraft } from "./policy-form.client";

export type PreviewProject = { slug: string; name: string };

export type PolicyPreviewState = {
  slug: string;
  setSlug: (s: string) => void;
  views: EffectivePipelinePreview[] | null;
  loading: boolean;
  error: string | null;
  /** Raw stage order of the previewed project's first pipeline (placement rail). */
  baseStages: string[];
};

// usePolicyPreview owns the live merge preview: it debounces the draft and asks
// the server (previewDraftPolicy → ApplyPolicies) to merge it into a real
// project's pipelines. Lifted to the manager so the form's placement rail and
// the preview pane share one selected project + one fetch.
export function usePolicyPreview(
  draft: PolicyDraft,
  projects: PreviewProject[],
): PolicyPreviewState {
  const [slug, setSlug] = useState(projects[0]?.slug ?? "");
  const [views, setViews] = useState<EffectivePipelinePreview[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const hasConfig = draft.configYaml.trim().length > 0;

  useEffect(() => {
    if (!slug || !hasConfig) {
      return;
    }
    let cancelled = false;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- live preview: flag in-flight while the debounced request runs
    setLoading(true);
    const t = setTimeout(async () => {
      const res = await previewDraftPolicy({
        slug,
        framework_ids: draft.appliesToAll ? [] : draft.frameworkIds,
        config_yaml: draft.configYaml,
        mode: draft.mode,
        position_before: draft.positionBefore,
        position_after: draft.positionAfter,
        priority: draft.priority,
      });
      if (cancelled) return;
      setLoading(false);
      if (res.ok) {
        setViews(res.data);
        setError(null);
      } else {
        setError(res.error);
        setViews(null);
      }
    }, 400);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [
    slug,
    hasConfig,
    draft.configYaml,
    draft.mode,
    draft.positionBefore,
    draft.positionAfter,
    draft.priority,
    draft.appliesToAll,
    draft.frameworkIds,
  ]);

  // Derive the surfaced state from "is there anything to preview" rather than
  // clearing in the effect — so a now-empty config/project never shows stale
  // views/error in the preview, footer summary, or placement rail.
  const active = !!slug && hasConfig;
  const baseStages = active ? (views?.[0]?.raw.stages ?? []) : [];

  return {
    slug,
    setSlug,
    views: active ? views : null,
    loading: active && loading,
    error: active ? error : null,
    baseStages,
  };
}
