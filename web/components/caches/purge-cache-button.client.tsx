"use client";

import { useState, useTransition } from "react";
import { Loader2, Trash2 } from "lucide-react";
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
import { purgeCache } from "@/server/actions/caches";

type Props = { slug: string; cacheID: string; cacheKey: string };

// Two-step purge: confirmation dialog then the action fires.
// Caches are recoverable — the next run will repopulate — but
// the rebuild can take real time (pnpm cold install is minutes),
// so the confirmation saves operators from misclicks on an LRU-
// sorted table where an "expensive" key and a throwaway key
// look identical.
export function PurgeCacheButton({ slug, cacheID, cacheKey }: Props) {
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();

  function onConfirm() {
    startTransition(async () => {
      const res = await purgeCache({ slug, cacheID });
      if (!res.ok) {
        toast.error(`purge cache: ${res.error}`);
        return;
      }
      toast.success(`Cache ${cacheKey} purged`);
      setOpen(false);
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Purge cache ${cacheKey}`}
          >
            <Trash2 className="h-4 w-4" aria-hidden />
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            Purge <span className="font-mono">{cacheKey}</span>?
          </DialogTitle>
          <DialogDescription>
            The next run that references this key will miss the cache and
            rebuild from scratch. Other jobs and projects are unaffected.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose
            render={
              <Button variant="outline" type="button">
                Cancel
              </Button>
            }
          />
          <Button
            variant="destructive"
            onClick={onConfirm}
            disabled={pending}
          >
            {pending ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              "Purge"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
