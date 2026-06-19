"use client";

import { useMemo, useState } from "react";
import { Search } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { SecretDialog } from "@/components/secrets/secret-dialog.client";
import { DeleteSecretButton } from "@/components/secrets/delete-secret-button.client";
import { secretSourceSummary } from "@/lib/secrets";
import type { Secret } from "@/types/api";

type Props = {
  secrets: Secret[];
  // External backends the server reports enabled, threaded into the
  // rotate dialog so an operator can repoint a secret's source.
  configuredSources: string[];
};

// GlobalSecretsTable wraps one page of the admin list with a
// client-side filter input. The list is server-paginated now (the
// page renders <Pagination> below this table), so the filter is
// SCOPED TO THE CURRENT PAGE — it narrows the rows already on screen,
// it doesn't search across pages. The "of N" counter reflects the
// page, not the absolute total, to keep that distinction honest.
export function GlobalSecretsTable({ secrets, configuredSources }: Props) {
  const [query, setQuery] = useState("");

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return secrets;
    return secrets.filter((s) => s.name.toLowerCase().includes(q));
  }, [secrets, query]);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={`Filter this page (${secrets.length})`}
            className="pl-8"
          />
        </div>
        <span className="text-xs text-muted-foreground tabular-nums">
          {filtered.length} of {secrets.length}
        </span>
      </div>

      {filtered.length === 0 ? (
        <div className="rounded-md border bg-muted/20 py-8 text-center text-sm text-muted-foreground">
          No secrets match &ldquo;{query}&rdquo;.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((s) => (
                <TableRow key={s.name}>
                  <TableCell className="font-mono">{s.name}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {secretSourceSummary(s)}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    <RelativeTime at={s.created_at} />
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    <RelativeTime at={s.updated_at} />
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="inline-flex items-center gap-1">
                      <SecretDialog
                        scope="global"
                        mode="rotate"
                        name={s.name}
                        configuredSources={configuredSources}
                        trigger={
                          <Button variant="ghost" size="sm">
                            Rotate
                          </Button>
                        }
                      />
                      <DeleteSecretButton scope="global" name={s.name} />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}
