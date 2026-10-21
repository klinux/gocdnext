"use client";

import { useState, useTransition } from "react";
import {
  Check,
  Copy,
  Database,
  Link2,
  Loader2,
  RotateCw,
  ShieldOff,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import {
  disableProjectCacheToken,
  rotateProjectCacheToken,
} from "@/server/actions/project-settings";

type Props = {
  slug: string;
  initialEnabled: boolean;
};

export function ProjectBuildCacheSettings({ slug, initialEnabled }: Props) {
  const [enabled, setEnabled] = useState(initialEnabled);
  const [creds, setCreds] = useState<{
    token: string;
    snippet: string;
    cacheURL: string;
  } | null>(null);
  const [copied, setCopied] = useState<"token" | "snippet" | "url" | null>(null);
  const [pending, startTransition] = useTransition();

  const onRotate = () => {
    startTransition(async () => {
      const res = await rotateProjectCacheToken({ slug });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setEnabled(true);
      setCreds({
        token: res.token,
        snippet: res.gradle_snippet,
        cacheURL: res.cache_url,
      });
      setCopied(null);
      toast.success(
        enabled ? "Build cache token rotated" : "Build cache enabled",
      );
    });
  };

  const onDisable = () => {
    startTransition(async () => {
      const res = await disableProjectCacheToken({ slug });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setEnabled(false);
      setCreds(null);
      setCopied(null);
      toast.success("Build cache disabled");
    });
  };

  const copyText = async (kind: "token" | "snippet" | "url", value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(kind);
      toast.success("Copied to clipboard");
      setTimeout(() => setCopied(null), 1500);
    } catch {
      toast.error("Copy failed — select and copy manually");
    }
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <Database className="size-4 text-muted-foreground" aria-hidden />
            <CardTitle className="text-base">Gradle build cache</CardTitle>
          </div>
          <Badge variant={enabled ? "default" : "secondary"}>
            {enabled ? "Enabled" : "Disabled"}
          </Badge>
        </div>
        <CardDescription>
          A remote, entry-addressable Gradle build cache served by gocdnext.
          Concurrent branches/PRs share task outputs by hash — no clobbering.
          Jobs using the <code className="rounded bg-muted px-1 py-0.5 text-xs">gradle</code>{" "}
          plugin are wired automatically; the snippet below is for manual setup.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {creds ? (
          <div className="rounded-md border border-amber-300 bg-amber-50 p-3 dark:border-amber-800/60 dark:bg-amber-950/40">
            <p className="text-sm font-medium text-amber-700 dark:text-amber-400">
              Copy the token now — it is shown only once.
            </p>
            <div className="mt-2 flex flex-col gap-2 sm:flex-row sm:items-center">
              <input
                readOnly
                value={creds.token}
                aria-label="Build cache token"
                className="flex-1 rounded-md border border-border bg-background px-2 py-1.5 font-mono text-xs"
              />
              <Button
                type="button"
                variant="outline"
                onClick={() => copyText("token", creds.token)}
              >
                {copied === "token" ? (
                  <Check className="size-3.5" aria-hidden />
                ) : (
                  <Copy className="size-3.5" aria-hidden />
                )}
                {copied === "token" ? "Copied" : "Copy token"}
              </Button>
            </div>
            <p className="mt-2 text-xs text-amber-700/90 dark:text-amber-400/90">
              For auto-wired <code className="rounded bg-muted px-1 py-0.5">gradle</code>{" "}
              plugin jobs you don&apos;t need this. For manual setup, store it as
              a CI secret <code className="rounded bg-muted px-1 py-0.5">GOCDNEXT_CACHE_TOKEN</code>{" "}
              and use the snippet below.
            </p>
            <div className="mt-2 flex flex-col gap-2 sm:flex-row">
              <Textarea
                readOnly
                value={creds.snippet}
                aria-label="Gradle buildCache snippet"
                className="min-h-40 flex-1 font-mono text-xs"
              />
              <div className="flex flex-row gap-2 sm:flex-col">
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => copyText("snippet", creds.snippet)}
                  className="sm:self-start"
                >
                  {copied === "snippet" ? (
                    <Check className="size-3.5" aria-hidden />
                  ) : (
                    <Copy className="size-3.5" aria-hidden />
                  )}
                  {copied === "snippet" ? "Copied" : "Copy snippet"}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => copyText("url", creds.cacheURL)}
                  className="sm:self-start"
                >
                  {copied === "url" ? (
                    <Check className="size-3.5" aria-hidden />
                  ) : (
                    <Link2 className="size-3.5" aria-hidden />
                  )}
                  {copied === "url" ? "Copied" : "Copy URL"}
                </Button>
              </div>
            </div>
          </div>
        ) : enabled ? (
          <p className="text-xs text-muted-foreground">
            The build cache is enabled. The token is not stored in the UI —
            rotate it to get a fresh token and the Gradle snippet. Jobs using
            the <code className="rounded bg-muted px-1 py-0.5 text-xs">gradle</code>{" "}
            plugin are auto-wired and need no token.
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">
            Generate a token to enable the remote build cache for this project.
            Keep the <code className="rounded bg-muted px-1 py-0.5 text-xs">cache:</code>{" "}
            block for <code className="rounded bg-muted px-1 py-0.5 text-xs">GRADLE_USER_HOME</code>{" "}
            dependencies.
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <Button onClick={onRotate} disabled={pending}>
            {pending ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              <RotateCw className="size-4" aria-hidden />
            )}
            {enabled ? "Rotate token" : "Enable build cache"}
          </Button>
          {enabled ? (
            <Button variant="destructive" onClick={onDisable} disabled={pending}>
              <ShieldOff className="size-4" aria-hidden />
              Disable
            </Button>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}
