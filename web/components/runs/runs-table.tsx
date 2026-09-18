import { ChevronDown, ChevronUp } from "lucide-react";
import type { Route } from "next";
import Link from "next/link";
import { RunStatusBadge } from "@/components/runs/run-status-badge";
import { CauseBadge } from "@/components/shared/cause-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import type { GlobalRunSummary, RunSummary } from "@/types/api";

// Any row we might want to render — either a GlobalRunSummary
// (global /runs, dashboard) or a plain RunSummary (project-scope
// runs where the project slug is redundant because it's in the
// URL). The table narrows per-variant below.
type Row = RunSummary | GlobalRunSummary;

type Props = {
	runs: Row[];
	// "global" shows the project slug alongside the pipeline name
	// (used by /runs and the dashboard). "project" omits the slug
	// because the surrounding page already makes the project
	// context obvious.
	variant?: "global" | "project";
	// Empty-state copy — callers customize per page (e.g.
	// "No runs match your filters." vs. "No runs yet for this
	// project.").
	emptyMessage?: string;
	// Server-side column sorting (opt-in; only /runs passes these).
	// `sort`/`dir` echo the current order; `sortParams` are the other
	// query params to preserve when a header is clicked; basePath is
	// where the header links point.
	sort?: string;
	dir?: "asc" | "desc";
	sortParams?: Record<string, string | undefined>;
	sortBasePath?: string;
};

// First click on a column starts with the direction people expect:
// newest/biggest first for time-like columns, A→Z for text.
const DEFAULT_DIR: Record<string, "asc" | "desc"> = {
	started: "desc",
	duration: "desc",
	counter: "desc",
	pipeline: "asc",
	status: "asc",
	cause: "asc",
};

// RunsTable is the single visual source of truth for "list of
// runs" across /runs, /projects/[slug]/runs, and the dashboard
// Recent activity card. Each variant hides one column but
// otherwise renders identically so the user's scan pattern
// carries between pages. Rows link to /runs/{id} via the
// Project/Pipeline cell (keyboard accessible) — we don't wrap
// the entire row in a Link because nested clickable regions
// (status pill, etc.) clash with row-level navigation.
export function RunsTable({
	runs,
	variant = "global",
	emptyMessage = "No runs to show.",
	sort,
	dir,
	sortParams,
	sortBasePath,
}: Props) {
	const sortable = Boolean(sortBasePath);
	const sortHref = (key: string): Route => {
		const q = new URLSearchParams();
		for (const [k, v] of Object.entries(sortParams ?? {})) {
			if (v != null && v !== "") q.set(k, v);
		}
		const nextDir =
			sort === key ? (dir === "asc" ? "desc" : "asc") : DEFAULT_DIR[key];
		q.set("sort", key);
		q.set("dir", nextDir);
		return `${sortBasePath}?${q.toString()}` as Route;
	};
	const SortHead = ({
		col,
		children,
		className,
	}: {
		col: string;
		children: React.ReactNode;
		className?: string;
	}) => (
		<TableHead className={className}>
			{sortable ? (
				<Link
					href={sortHref(col)}
					className="inline-flex items-center gap-1 hover:text-foreground"
					aria-sort={
						sort === col
							? dir === "asc"
								? "ascending"
								: "descending"
							: undefined
					}
				>
					{children}
					{sort === col ? (
						dir === "asc" ? (
							<ChevronUp className="size-3.5" aria-hidden />
						) : (
							<ChevronDown className="size-3.5" aria-hidden />
						)
					) : null}
				</Link>
			) : (
				children
			)}
		</TableHead>
	);
	if (runs.length === 0) {
		return (
			<div className="rounded-lg border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
				{emptyMessage}
			</div>
		);
	}

	const showProject = variant === "global";

	return (
		<div className="overflow-hidden rounded-lg border border-border bg-card">
			<Table>
				<TableHeader>
					<TableRow>
						<SortHead col="status" className="w-[120px]">
							Status
						</SortHead>
						<SortHead col="pipeline">
							{showProject ? "Project / Pipeline" : "Pipeline"}
						</SortHead>
						<SortHead col="counter" className="w-20">
							#
						</SortHead>
						<SortHead col="cause" className="w-28">
							Cause
						</SortHead>
						<SortHead col="started" className="w-36">
							Started
						</SortHead>
						<SortHead col="duration" className="w-28">
							Duration
						</SortHead>
					</TableRow>
				</TableHeader>
				<TableBody>
					{runs.map((r) => {
						const dur = formatDurationSeconds(
							durationBetween(r.started_at, r.finished_at),
						);
						const isGlobal = isGlobalRun(r);
						return (
							<TableRow key={r.id} className="font-mono text-xs">
								<TableCell>
									<RunStatusBadge
										status={r.status}
										cancelReason={r.cancel_reason}
										supersededBy={r.superseded_by}
										queueReason={r.queue_reason}
									/>
								</TableCell>
								<TableCell className="truncate">
									<Link
										href={`/runs/${r.id}` as Route}
										className="hover:underline"
									>
										{showProject && isGlobal ? (
											<>
												<span className="text-muted-foreground">
													{r.project_slug}
												</span>{" "}
												/ {r.pipeline_name}
											</>
										) : (
											r.pipeline_name
										)}
									</Link>
								</TableCell>
								<TableCell className="font-semibold">#{r.counter}</TableCell>
								<TableCell>
									<CauseBadge cause={r.cause} />
								</TableCell>
								<TableCell>
									<RelativeTime at={r.started_at ?? r.created_at} />
								</TableCell>
								<TableCell>{dur}</TableCell>
							</TableRow>
						);
					})}
				</TableBody>
			</Table>
		</div>
	);
}

// isGlobalRun narrows Row to GlobalRunSummary so the project
// column renders safely when the variant asks for it. A plain
// RunSummary in global mode degrades to pipeline-only (same as
// project variant), but the common case is that /runs always
// hands us globals.
function isGlobalRun(r: Row): r is GlobalRunSummary {
	return "project_slug" in r && typeof r.project_slug === "string";
}
