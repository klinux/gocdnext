"use client";

import { useEffect, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { StageSection } from "@/components/runs/stage-section";
import { RunArtifacts, fetchArtifacts } from "@/components/runs/run-artifacts.client";
import { RunTests, fetchTests } from "@/components/runs/run-tests.client";
import { isTerminalStatus } from "@/lib/status";
import type { RunDetail } from "@/types/api";

const TAB_VALUES = ["jobs", "tests", "artifacts"] as const;
type TabValue = (typeof TAB_VALUES)[number];

const TESTS_POLL_MS = 5_000;
const ARTIFACTS_POLL_MS = 5_000;

type Props = {
  runId: string;
  run: RunDetail;
  apiBaseURL: string;
};

// RunTabs splits the run-detail body — Jobs/Tests/Artifacts — into a
// shadcn Tabs strip. Counts on the labels come from sibling react-
// query reads with the SAME queryKey the inner components use, so
// the cache dedupes the request: when the user switches into the
// Tests panel the inner useQuery resolves from cache instantly
// instead of round-tripping again.
//
// State is also URL-persisted via ?tab=… so toast deep-links from
// "Open" / refresh / browser-back land on the panel the user was on.
export function RunTabs({ runId, run, apiBaseURL }: Props) {
  const router = useRouter();
  const params = useSearchParams();
  const [tab, setTab] = useState<TabValue>(() => parseTab(params?.get("tab")));

  // Sync URL when tab changes — replace, not push, so back-button
  // returns to the page that linked HERE, not to the previous tab.
  useEffect(() => {
    const current = params?.get("tab");
    if (current === tab) return;
    const next = new URLSearchParams(params?.toString() ?? "");
    if (tab === "jobs") next.delete("tab");
    else next.set("tab", tab);
    const qs = next.toString();
    router.replace(qs ? `?${qs}` : "?", { scroll: false });
  }, [tab, params, router]);

  // Sibling queries — same keys as the components inside each panel
  // so the data is shared via react-query cache. We only read counts
  // here; full state lives in the panel components.
  const testsQuery = useQuery({
    queryKey: ["run-tests", runId],
    queryFn: () => fetchTests(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(run.status) ? false : TESTS_POLL_MS,
    staleTime: 30_000,
  });
  const artifactsQuery = useQuery({
    queryKey: ["run-artifacts", runId],
    queryFn: () => fetchArtifacts(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(run.status) ? false : ARTIFACTS_POLL_MS,
    staleTime: 60_000,
  });

  const testCount = (testsQuery.data?.summaries ?? []).reduce(
    (acc, s) => acc + s.total,
    0,
  );
  const artifactCount = artifactsQuery.data?.length ?? 0;

  return (
    <Tabs value={tab} onValueChange={(v) => setTab(parseTab(v))}>
      <TabsList>
        <TabsTrigger value="jobs">Jobs</TabsTrigger>
        <TabsTrigger value="tests">
          Tests
          {testCount > 0 ? (
            <span className="ml-1 rounded-full bg-muted px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
              {testCount}
            </span>
          ) : null}
        </TabsTrigger>
        <TabsTrigger value="artifacts">
          Artifacts
          {artifactCount > 0 ? (
            <span className="ml-1 rounded-full bg-muted px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
              {artifactCount}
            </span>
          ) : null}
        </TabsTrigger>
      </TabsList>

      <TabsContent value="jobs" className="mt-4">
        {run.stages.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This run has no stages.
          </p>
        ) : (
          <div className="space-y-8">
            {run.stages.map((s) => (
              <StageSection key={s.id} stage={s} runID={runId} />
            ))}
          </div>
        )}
      </TabsContent>

      <TabsContent value="tests" className="mt-4">
        <RunTests
          runId={runId}
          runStatus={run.status}
          run={run}
          apiBaseURL={apiBaseURL}
        />
      </TabsContent>

      <TabsContent value="artifacts" className="mt-4">
        <RunArtifacts
          runId={runId}
          runStatus={run.status}
          apiBaseURL={apiBaseURL}
        />
      </TabsContent>
    </Tabs>
  );
}

function parseTab(raw: string | null | undefined): TabValue {
  // Whitelist instead of trusting the URL — protects against a
  // typo'd ?tab=foo silently rendering nothing.
  return TAB_VALUES.includes(raw as TabValue) ? (raw as TabValue) : "jobs";
}
