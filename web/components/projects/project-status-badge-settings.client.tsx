"use client";

import { useState, useTransition } from "react";
import {
  BadgeCheck,
  Check,
  Copy,
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
  disableProjectBadgeToken,
  rotateProjectBadgeToken,
} from "@/server/actions/project-settings";

type Props = {
  slug: string;
  initialEnabled: boolean;
};

export function ProjectStatusBadgeSettings({ slug, initialEnabled }: Props) {
  const [enabled, setEnabled] = useState(initialEnabled);
  const [snippet, setSnippet] = useState<{
    markdown: string;
    badgeURL: string;
  } | null>(null);
  const [copied, setCopied] = useState<"markdown" | "link" | null>(null);
  const [pending, startTransition] = useTransition();

  const onRotate = () => {
    startTransition(async () => {
      const res = await rotateProjectBadgeToken({ slug });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setEnabled(true);
      setSnippet({ markdown: res.markdown, badgeURL: res.badge_url });
      setCopied(null);
      toast.success(enabled ? "Badge token rotated" : "Badge token generated");
    });
  };

  const onDisable = () => {
    startTransition(async () => {
      const res = await disableProjectBadgeToken({ slug });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setEnabled(false);
      setSnippet(null);
      setCopied(null);
      toast.success("Status badge disabled");
    });
  };

  const copyText = async (kind: "markdown" | "link", value: string) => {
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
            <BadgeCheck className="size-4 text-muted-foreground" aria-hidden />
            <CardTitle className="text-base">README status badge</CardTitle>
          </div>
          <Badge variant={enabled ? "default" : "secondary"}>
            {enabled ? "Enabled" : "Disabled"}
          </Badge>
        </div>
        <CardDescription>
          Publish a project badge like{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            ![build](...)
          </code>{" "}
          in a README. The URL contains a project-scoped token; rotate it to
          invalidate old embeds.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {snippet ? (
          <div className="rounded-md border border-amber-300 bg-amber-50 p-3 dark:border-amber-800/60 dark:bg-amber-950/40">
            <p className="text-sm font-medium text-amber-700 dark:text-amber-400">
              Copy this badge link now — the token is shown only once.
            </p>
            <div className="mt-2 flex flex-col gap-2 sm:flex-row">
              <Textarea
                readOnly
                value={snippet.markdown}
                aria-label="Badge Markdown"
                className="min-h-16 flex-1 font-mono text-xs"
              />
              <Button
                type="button"
                variant="outline"
                onClick={() => copyText("markdown", snippet.markdown)}
                className="sm:self-start"
              >
                {copied === "markdown" ? (
                  <Check className="size-3.5" aria-hidden />
                ) : (
                  <Copy className="size-3.5" aria-hidden />
                )}
                {copied === "markdown" ? "Copied" : "Copy Markdown"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => copyText("link", snippet.badgeURL)}
                className="sm:self-start"
              >
                {copied === "link" ? (
                  <Check className="size-3.5" aria-hidden />
                ) : (
                  <Link2 className="size-3.5" aria-hidden />
                )}
                {copied === "link" ? "Copied" : "Copy link"}
              </Button>
            </div>
          </div>
        ) : enabled ? (
          <p className="text-xs text-muted-foreground">
            The badge is enabled, but the plaintext token is not stored in the
            UI. Rotate the token to get a fresh Markdown snippet.
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">
            Disabled projects always return an anonymous unknown badge. Generate
            a token to publish this project&apos;s current build status.
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <Button onClick={onRotate} disabled={pending}>
            {pending ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              <RotateCw className="size-4" aria-hidden />
            )}
            {enabled ? "Rotate token" : "Generate badge token"}
          </Button>
          {enabled ? (
            <Button variant="destructive" onClick={onDisable} disabled={pending}>
              <ShieldOff className="size-4" aria-hidden />
              Disable badge
            </Button>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}
