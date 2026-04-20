"use client";

import Link from "next/link";
import type { Route } from "next";
import { Handle, Position, type NodeProps } from "@xyflow/react";
import { GitBranch } from "lucide-react";

import type { VSMNode as VSMNodeT } from "@/types/api";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";

type Data = { pipeline: VSMNodeT; slug: string };

export function PipelineNode({ data }: NodeProps) {
  const { pipeline, slug } = data as Data;
  const latest = pipeline.latest_run;

  return (
    <div className="min-w-[220px] rounded-md border border-border bg-card p-3 shadow-sm">
      <Handle type="target" position={Position.Left} />
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-sm font-semibold">{pipeline.name}</span>
        {latest ? (
          <StatusBadge status={latest.status} />
        ) : (
          <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
            no runs
          </span>
        )}
      </div>

      {pipeline.git_materials && pipeline.git_materials.length > 0 ? (
        <div className="mt-1.5 flex items-center gap-1 truncate text-[11px] text-muted-foreground">
          <GitBranch className="h-3 w-3 shrink-0" aria-hidden />
          <span className="truncate" title={pipeline.git_materials[0].url}>
            {stripProtocol(pipeline.git_materials[0].url)}
            {pipeline.git_materials[0].branch
              ? `@${pipeline.git_materials[0].branch}`
              : ""}
          </span>
        </div>
      ) : null}

      {latest ? (
        <div className="mt-2 flex items-center justify-between text-[11px] text-muted-foreground">
          <Link
            href={`/runs/${latest.id}` as Route}
            className="font-mono text-primary hover:underline"
          >
            #{latest.counter}
          </Link>
          <RelativeTime at={latest.started_at ?? latest.created_at} />
        </div>
      ) : null}

      <Handle type="source" position={Position.Right} />
    </div>
  );
}

function stripProtocol(url: string): string {
  return url.replace(/^https?:\/\//, "");
}
