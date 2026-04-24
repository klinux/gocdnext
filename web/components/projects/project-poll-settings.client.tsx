"use client";

import { useState, useTransition } from "react";
import { Clock, Loader2, Save } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { setProjectPollInterval } from "@/server/actions/project-settings";

type Props = {
  slug: string;
  // initialIntervalNs is the current project-level poll interval
  // in nanoseconds as returned by the API; 0 means "polling off".
  initialIntervalNs: number;
  hasScmSource: boolean;
};

// Format a Go-style duration string from nanoseconds. We stick to
// the composition Go's time.Duration.String would emit for the
// round values users typically pick — "5m", "1h", "2h30m" — so
// what lands in the field matches what the YAML spec accepts and
// what the backend echoes back.
function formatDuration(ns: number): string {
  if (ns <= 0) return "";
  const seconds = Math.floor(ns / 1_000_000_000);
  if (seconds === 0) return "";

  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  let out = "";
  if (h > 0) out += `${h}h`;
  if (m > 0) out += `${m}m`;
  if (s > 0 && h === 0) out += `${s}s`;
  return out || "";
}

export function ProjectPollSettings({
  slug,
  initialIntervalNs,
  hasScmSource,
}: Props) {
  const [value, setValue] = useState<string>(formatDuration(initialIntervalNs));
  const [pending, startTransition] = useTransition();

  const onSave = () => {
    startTransition(async () => {
      const res = await setProjectPollInterval({
        slug,
        interval: value.trim(),
      });
      if (res.ok) {
        toast.success(
          value.trim() === ""
            ? "Polling disabled"
            : `Polling every ${value.trim()}`,
        );
      } else {
        toast.error(res.error);
      }
    });
  };

  const disabled = !hasScmSource || pending;

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <Clock className="size-4 text-muted-foreground" aria-hidden />
          <CardTitle className="text-base">Branch polling</CardTitle>
        </div>
        <CardDescription>
          Periodically check the default branch for new commits and dispatch a
          run when HEAD advances. Useful when the repo can&apos;t reach us via
          webhook (corporate firewalls, self-hosted Git behind VPN). Per-
          pipeline{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            poll_interval:
          </code>{" "}
          in a declared git material overrides this project-level default.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {!hasScmSource ? (
          <p className="text-sm text-muted-foreground">
            Connect a repo to this project to enable polling.
          </p>
        ) : null}

        <div className="space-y-2">
          <Label htmlFor="poll-interval">Interval</Label>
          <div className="flex items-center gap-2">
            <Input
              id="poll-interval"
              type="text"
              inputMode="text"
              placeholder="e.g. 5m, 30m, 1h (leave empty to disable)"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              disabled={disabled}
              className="max-w-xs"
              aria-describedby="poll-interval-help"
            />
            <Button onClick={onSave} disabled={disabled}>
              {pending ? (
                <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
              ) : (
                <Save className="mr-2 size-4" aria-hidden />
              )}
              Save
            </Button>
          </div>
          <p
            id="poll-interval-help"
            className="text-xs text-muted-foreground"
          >
            Go duration format (1m minimum, 24h maximum). Empty disables
            polling entirely.
          </p>
        </div>
      </CardContent>
    </Card>
  );
}
