"use client";

import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  TIER_RANK,
  type Tier,
  cfrTier,
  fmtDuration,
  fmtFreq,
} from "@/lib/dora";
import { cn } from "@/lib/utils";
import type { DoraGroup } from "@/server/queries/analytics";

import { teamTier } from "./dora-metrics";
import { TierChip } from "./dora-hero-cards";

// One leaderboard row: raw numerics for sorting + the group for display.
type Row = {
  name: string;
  deploys: number;
  freqPerDay: number;
  leadSec: number;
  cfr: number;
  mttrSec: number;
  tier: Tier;
};

type SortKey = "name" | "deploys" | "freqPerDay" | "leadSec" | "cfr" | "mttrSec" | "tier";

const COLS: { key: SortKey; label: string; num: boolean }[] = [
  { key: "name", label: "Time", num: false },
  { key: "deploys", label: "Deploys", num: true },
  { key: "freqPerDay", label: "Deploy freq", num: true },
  { key: "leadSec", label: "Lead time", num: true },
  { key: "cfr", label: "Change failure", num: true },
  { key: "mttrSec", label: "Restore (MTTR)", num: true },
  { key: "tier", label: "Faixa", num: true },
];

function cfrTone(rate: number): string {
  const t = cfrTier(rate);
  if (t === "elite" || t === "high") return "text-status-success";
  if (t === "medium") return "text-status-warning";
  return "text-status-failed";
}

function toRow(g: DoraGroup): Row {
  return {
    name: g.group,
    deploys: g.deploys_total,
    freqPerDay: g.deploy_freq_per_day,
    leadSec: g.lead_time_p50_seconds,
    cfr: g.change_failure_rate,
    mttrSec: g.mttr_p50_seconds,
    tier: teamTier(g),
  };
}

// DoraLeaderboard ranks teams across the four DORA metrics. Click a header to
// sort; click again to flip direction. Default: Faixa (tier) descending —
// best performers first.
export function DoraLeaderboard({ teams }: { teams: DoraGroup[] }) {
  const [sortKey, setSortKey] = useState<SortKey>("tier");
  const [dir, setDir] = useState<1 | -1>(-1);

  const rows = useMemo(() => {
    const base = teams.map(toRow);
    return base.sort((a, b) => {
      if (sortKey === "name") return dir * a.name.localeCompare(b.name);
      const av = sortKey === "tier" ? TIER_RANK[a.tier] : a[sortKey];
      const bv = sortKey === "tier" ? TIER_RANK[b.tier] : b[sortKey];
      return dir * (av - bv);
    });
  }, [teams, sortKey, dir]);

  function onSort(k: SortKey) {
    if (k === sortKey) {
      setDir((d) => (d === 1 ? -1 : 1));
    } else {
      setSortKey(k);
      setDir(k === "name" ? 1 : -1);
    }
  }

  return (
    <div className="overflow-hidden rounded-xl ring-1 ring-foreground/10">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            {COLS.map((c) => (
              <TableHead
                key={c.key}
                aria-sort={
                  sortKey === c.key ? (dir === 1 ? "ascending" : "descending") : "none"
                }
                className={cn(
                  "cursor-pointer select-none font-mono text-[10.5px] font-semibold uppercase tracking-wide whitespace-nowrap",
                  c.num && "text-right",
                  sortKey === c.key ? "text-brand-500" : "text-muted-foreground",
                )}
                onClick={() => onSort(c.key)}
              >
                <span className={cn("inline-flex items-center gap-1", c.num && "flex-row-reverse")}>
                  {c.label}
                  {sortKey === c.key ? (
                    dir === 1 ? (
                      <ArrowUp className="size-3" aria-hidden />
                    ) : (
                      <ArrowDown className="size-3" aria-hidden />
                    )
                  ) : null}
                </span>
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.name} className="font-mono">
              <TableCell className="font-semibold">
                <span className="text-brand-500">team:</span>
                {r.name}
              </TableCell>
              <TableCell className="text-right">{r.deploys}</TableCell>
              <TableCell className="text-right">{fmtFreq(r.freqPerDay, "sem")}</TableCell>
              <TableCell className="text-right">{fmtDuration(r.leadSec)}</TableCell>
              <TableCell className={cn("text-right", cfrTone(r.cfr))}>
                {Math.round(r.cfr * 100)}%
              </TableCell>
              <TableCell className="text-right">{fmtDuration(r.mttrSec)}</TableCell>
              <TableCell className="text-right">
                <TierChip tier={r.tier} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
