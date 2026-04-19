"use client";

import { useState, useTransition, type ReactElement } from "react";
import { Plus, RotateCcw, AlertCircle, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { secretNameSchema, setSecret } from "@/server/actions/secrets";

type Props = {
  slug: string;
  // When `name` is set, the dialog is in "rotate" mode — name is locked and
  // the submit button reads "Update value" instead of "Create".
  mode?: "create" | "rotate";
  name?: string;
  // `render`-style prop for base-ui: accepts a single element, not a ReactNode.
  trigger?: ReactElement;
};

// SecretDialog handles both creating a new secret (trigger = "New secret"
// button) and rotating an existing value (trigger = "Rotate" on a row).
// Rotate mode reuses setSecret because the server endpoint is an upsert.
export function SecretDialog({ slug, mode = "create", name = "", trigger }: Props) {
  const [open, setOpen] = useState(false);
  const [formName, setFormName] = useState(name);
  const [value, setValue] = useState("");
  const [clientError, setClientError] = useState<string | null>(null);
  const [pending, startTransition] = useTransition();

  const rotating = mode === "rotate";

  function resetAndClose() {
    setFormName(name);
    setValue("");
    setClientError(null);
    setOpen(false);
  }

  function onSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setClientError(null);

    // Re-check the name here so the error lands next to the field instead of
    // in a toast — better UX than waiting for the server round-trip.
    const nameCheck = secretNameSchema.safeParse(formName);
    if (!nameCheck.success) {
      setClientError(nameCheck.error.issues[0]?.message ?? "invalid name");
      return;
    }
    if (value.length === 0) {
      setClientError("value cannot be empty");
      return;
    }

    startTransition(async () => {
      const res = await setSecret({ slug, name: formName, value });
      if (!res.ok) {
        toast.error(`set secret: ${res.error}`);
        return;
      }
      toast.success(
        res.created ? `Secret ${formName} created` : `Secret ${formName} updated`,
      );
      resetAndClose();
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) resetAndClose();
        else setOpen(true);
      }}
    >
      <DialogTrigger render={
        trigger ??
          <Button size="sm">
            <Plus className="h-4 w-4" aria-hidden />
            New secret
          </Button>
      } />
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {rotating ? (
              <>
                Rotate <span className="font-mono">{name}</span>
              </>
            ) : (
              "New secret"
            )}
          </DialogTitle>
          <DialogDescription>
            {rotating
              ? "Set a new value for this secret. The old value is replaced immediately."
              : "Values are encrypted at rest (AES-256-GCM) and never echoed back by the API."}
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={onSubmit} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="secret-name">Name</Label>
            <Input
              id="secret-name"
              name="name"
              autoComplete="off"
              spellCheck={false}
              readOnly={rotating}
              value={formName}
              onChange={(e) => setFormName(e.target.value)}
              className={cn("font-mono", rotating && "bg-muted")}
              placeholder="GH_TOKEN"
            />
            <p className="text-[11px] text-muted-foreground">
              Letters, digits, underscore. Must start with a letter.
            </p>
          </div>

          <div className="space-y-2">
            <Label htmlFor="secret-value">Value</Label>
            <Textarea
              id="secret-value"
              name="value"
              autoComplete="off"
              spellCheck={false}
              rows={4}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              className="font-mono text-sm"
              placeholder="ghp_..."
            />
            <p className="text-[11px] text-muted-foreground">
              Multi-line values (PEM keys, etc.) are OK. Not visible after saving.
            </p>
          </div>

          {clientError ? (
            <div
              role="alert"
              className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive"
            >
              <AlertCircle className="mt-0.5 h-3.5 w-3.5" aria-hidden />
              <span>{clientError}</span>
            </div>
          ) : null}

          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button">Cancel</Button>} />
            <Button type="submit" disabled={pending}>
              {pending ? <Loader2 className="h-4 w-4 animate-spin" /> : rotating ? (
                <>
                  <RotateCcw className="h-4 w-4" aria-hidden />
                  Update value
                </>
              ) : (
                "Create"
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
