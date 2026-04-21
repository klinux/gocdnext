"use client";

import { useState } from "react";
import { AlertTriangle, Check, Copy } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

type Props = {
  open: boolean;
  onOpenChange: (next: boolean) => void;
  secret: string;
  // Rendered under the title — lets the caller explain
  // whether this is a fresh bind ("created") or a rotation
  // ("rotated, old secret is now dead").
  title?: string;
  subtitle?: string;
  // Distinguishes "first-time generation" (no previous secret,
  // nothing to invalidate) from "rotation" (old secret now dead,
  // 401 until the provider is updated). Default is "rotate"
  // because rotation is the riskier case; the create flow must
  // explicitly pass "create" to get the softer copy.
  variant?: "create" | "rotate";
};

// WebhookSecretDialog is the "copy this once, we won't show it again" UI.
// Used by both new-project (when backend auto-generated a secret) and the
// rotation button on the project page. Keeps the copy-button state + the
// one-shot warning behaviour in one place so the two call sites stay
// consistent.
export function WebhookSecretDialog({
  open,
  onOpenChange,
  secret,
  title = "Webhook secret",
  subtitle,
  variant = "rotate",
}: Props) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
      toast.success("Copied to clipboard");
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error("Copy failed — select and copy manually");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {subtitle ??
              "Copy this value into your provider's webhook configuration now — it won't be shown again."}
          </DialogDescription>
        </DialogHeader>

        <div className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-xs">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-amber-600" />
          <p className="text-foreground/80">
            {variant === "create"
              ? "This is the only time the plaintext is displayed. Closing this dialog discards it from the page — register it in the provider's webhook configuration before pushing."
              : "This is the only time the plaintext is displayed. Closing this dialog discards it from the page. Any existing webhook signed with the previous secret will start returning 401 until you update the provider with the new value."}
          </p>
        </div>

        <div className="flex min-w-0 items-center gap-2">
          <code className="min-w-0 flex-1 break-all rounded-md border bg-muted/40 px-3 py-2 font-mono text-xs">
            {secret}
          </code>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={copy}
            aria-label="Copy webhook secret"
          >
            {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>

        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>Done</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
