"use client";

import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";

type Props = {
  endpoint: string;
};

// WebhookEndpointRow renders a copy-paste-ready webhook URL with
// a small clipboard button. A client component because the
// Clipboard API + hover state need the browser. Falls back to a
// plaintext "set GOCDNEXT_PUBLIC_BASE to see this" when the
// server hasn't been configured with a public base yet.
export function WebhookEndpointRow({ endpoint }: Props) {
  const [copied, setCopied] = useState(false);

  if (!endpoint) {
    return (
      <p className="text-xs text-muted-foreground">
        Configure <code className="rounded bg-muted px-1">GOCDNEXT_PUBLIC_BASE</code>{" "}
        to enable auto-register.
      </p>
    );
  }

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(endpoint);
      setCopied(true);
      toast.success("Copied to clipboard");
      setTimeout(() => setCopied(false), 1500);
    } catch {
      toast.error("Clipboard permission denied");
    }
  };

  return (
    <div className="flex items-center gap-2">
      <code className="truncate rounded bg-muted px-2 py-1 text-xs font-mono">
        {endpoint}
      </code>
      <Button
        variant="ghost"
        size="sm"
        onClick={copy}
        aria-label="Copy webhook URL"
      >
        {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
      </Button>
    </div>
  );
}
