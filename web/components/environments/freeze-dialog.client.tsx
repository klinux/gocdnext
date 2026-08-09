"use client";

import {
  type ReactNode,
  useId,
  useMemo,
  useState,
  useTransition,
} from "react";
import { useRouter } from "next/navigation";
import { Loader2, Snowflake } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
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
import { freezeEnvironment } from "@/server/actions/environments";

type Props = {
  slug: string;
  trigger: ReactNode;
  // When set, the dialog freezes THIS environment and the name field is locked
  // (the per-card affordance). Absent → the page-level control, where the
  // operator types the name: environments are created lazily at their first
  // deploy, so freezing one pre-emptively means naming an environment that has
  // no card yet.
  environment?: string;
  // Best-effort suggestions from environments the page already knows about
  // (deployed or currently frozen). Still free text: a freeze can name an
  // environment before its first deploy creates a row.
  environmentSuggestions?: string[];
};

const REASON_MAX = 500;
const EMPTY_ENVIRONMENT_SUGGESTIONS: string[] = [];

// FreezeDialog collects the REQUIRED reason and puts an environment under a
// change-freeze (#202).
//
// The reason is mandatory, not a nicety: it is what every operator sees when
// their production deploy is refused, and "frozen since Tuesday, nobody knows
// why" is the failure this feature exists to prevent.
//
// Controls are INLINE (plain buttons, an Input, a Textarea) — deliberately no
// DropdownMenu or Select inside the Dialog, which crashes in this stack (see
// policy-form.client.tsx).
export function FreezeDialog({
  slug,
  trigger,
  environment,
  environmentSuggestions = EMPTY_ENVIRONMENT_SUGGESTIONS,
}: Props) {
  const router = useRouter();
  const nameLocked = environment !== undefined;
  const rawSuggestionsID = useId();

  const [open, setOpen] = useState(false);
  const [name, setName] = useState(environment ?? "");
  const [reason, setReason] = useState("");
  const [pending, startTransition] = useTransition();
  const suggestions = useMemo(() => {
    const seen = new Set<string>();
    const values: string[] = [];
    for (const candidate of environmentSuggestions) {
      const value = candidate.trim();
      if (value === "" || seen.has(value)) continue;
      seen.add(value);
      values.push(value);
    }
    return values;
  }, [environmentSuggestions]);
  const suggestionsID = suggestions.length > 0 ? rawSuggestionsID : undefined;

  function submit() {
    startTransition(async () => {
      const res = await freezeEnvironment({ slug, name: name.trim(), reason });
      if (!res.ok) {
        toast.error(`Freeze ${name.trim() || "environment"}: ${res.error}`);
        return;
      }
      toast.success(
        `${name.trim()} is frozen — no jobs targeting it will be admitted`,
      );
      setOpen(false);
      setReason("");
      if (!nameLocked) setName("");
      router.refresh();
    });
  }

  const canSubmit = name.trim() !== "" && reason.trim() !== "" && !pending;

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      {/* base-ui's Trigger renders INTO the given element, so it needs a
          ReactElement rather than arbitrary ReactNode — same cast the deploy
          target dialog uses. */}
      <DialogTrigger render={trigger as React.ReactElement} />
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Snowflake className="size-4" aria-hidden />
            Freeze {nameLocked ? name : "an environment"}
          </DialogTitle>
          <DialogDescription>
            While frozen, gocdnext admits no promotion to this environment:
            approving a gate that governs it is refused, jobs targeting it (a
            deploy or a migration) stay queued, and rollback is refused. A job
            already admitted keeps running — the freeze stops what starts next.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          {!nameLocked ? (
            <div className="space-y-1.5">
              <Label htmlFor="freeze-env">Environment</Label>
              <Input
                id="freeze-env"
                list={suggestionsID}
                value={name}
                onValueChange={setName}
                placeholder="production"
                autoComplete="off"
              />
              {suggestionsID ? (
                <datalist id={suggestionsID}>
                  {suggestions.map((suggestion) => (
                    <option key={suggestion} value={suggestion} />
                  ))}
                </datalist>
              ) : null}
              <p className="text-xs text-muted-foreground">
                It doesn&apos;t have to exist yet — freezing a name blocks the
                first deploy that would create it.
              </p>
            </div>
          ) : null}

          <div className="space-y-1.5">
            <Label htmlFor="freeze-reason">Reason</Label>
            <Textarea
              id="freeze-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              maxLength={REASON_MAX}
              rows={3}
              placeholder="month-end close — no prod deploys until 2026-08-03"
            />
            <p className="text-xs text-muted-foreground">
              Shown to maintainers and admins, and recorded in the audit log.
              Viewers see only that the environment is frozen.{" "}
              {reason.length}/{REASON_MAX}
            </p>
          </div>
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            onClick={() => setOpen(false)}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={!canSubmit}>
            {pending ? (
              <Loader2 className="mr-1 size-4 animate-spin" aria-hidden />
            ) : null}
            Freeze
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
