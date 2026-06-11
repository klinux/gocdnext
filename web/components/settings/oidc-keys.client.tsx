"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { KeyRound, RefreshCw, ShieldAlert } from "lucide-react";
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { rotateOIDCKey } from "@/server/actions/oidc-keys";
import type { OIDCKey } from "@/types/api";

type Props = { keys: OIDCKey[] };

type KeyStatus = "active" | "retired" | "revoked";

function statusOf(k: OIDCKey): KeyStatus {
  if (k.revoked_at) return "revoked";
  if (k.retired_at) return "retired";
  return "active";
}

export function OIDCKeysManager({ keys }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [emergencyOpen, setEmergencyOpen] = useState(false);
  const [typedKid, setTypedKid] = useState("");

  const activeKey = keys.find((k) => statusOf(k) === "active");

  const rotate = (mode: "graceful" | "emergency") => {
    startTransition(async () => {
      const res = await rotateOIDCKey({ mode });
      if (res.ok) {
        setEmergencyOpen(false);
        setTypedKid("");
        toast.success(`Rotated (${mode}) — new kid ${res.data.kid}`, {
          description: res.data.note || undefined,
        });
        router.refresh();
      } else {
        toast.error(`Rotation failed: ${res.error}`);
      }
    });
  };

  const onGraceful = () => {
    if (
      !confirm(
        "Rotate the signing key? The current key keeps verifying in the JWKS until in-flight tokens expire — zero impact on running jobs.",
      )
    ) {
      return;
    }
    rotate("graceful");
  };

  if (keys.length === 0) {
    return (
      <Card>
        <CardHeader className="flex-row items-start gap-3 space-y-0">
          <KeyRound className="mt-0.5 size-5 shrink-0 text-muted-foreground" />
          <div>
            <CardTitle className="text-base">
              The issuer has never generated a key
            </CardTitle>
            <CardDescription className="mt-1">
              The OIDC issuer activates when the server has a public base URL
              and an auth encryption key configured. Once it boots enabled, the
              signing key appears here automatically.
            </CardDescription>
          </div>
        </CardHeader>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between space-y-0">
        <div>
          <CardTitle className="text-base">Signing keys</CardTitle>
          <CardDescription className="mt-1">
            Rotation history, newest first. Key material never leaves the
            server — the JWKS endpoint is the only public-key surface.
          </CardDescription>
        </div>
        <div className="flex shrink-0 gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={onGraceful}
            disabled={pending}
          >
            <RefreshCw className="size-4" aria-hidden />
            Rotate key
          </Button>
          {activeKey ? (
            <Button
              variant="destructive"
              size="sm"
              onClick={() => setEmergencyOpen(true)}
              disabled={pending}
            >
              <ShieldAlert className="size-4" aria-hidden />
              Emergency rotate
            </Button>
          ) : null}
        </div>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>kid</TableHead>
              <TableHead>alg</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Ended</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.map((k) => {
              const status = statusOf(k);
              return (
                <TableRow key={k.id}>
                  <TableCell className="font-mono text-xs">{k.kid}</TableCell>
                  <TableCell className="text-xs">{k.alg}</TableCell>
                  <TableCell>
                    <StatusBadge status={status} />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {fmtDate(k.created_at)}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.revoked_at
                      ? `revoked ${fmtDate(k.revoked_at)}`
                      : k.retired_at
                        ? `retired ${fmtDate(k.retired_at)}`
                        : "—"}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </CardContent>

      <Dialog open={emergencyOpen} onOpenChange={setEmergencyOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <ShieldAlert className="size-5 text-destructive" aria-hidden />
              Emergency rotation
            </DialogTitle>
            <DialogDescription>
              This is the key-compromise response. The active key is revoked
              and removed from the JWKS <strong>immediately</strong>: every
              already-issued token stops verifying, and jobs mid-exchange will
              fail until they rerun. Verifiers may cache the old JWKS for up
              to 5 minutes.
            </DialogDescription>
            <p className="text-sm text-muted-foreground">
              If the key is not compromised, use the regular rotate — it has
              zero impact on in-flight tokens.
            </p>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="oidc-emergency-kid">
              Type the active kid to confirm
            </Label>
            <Input
              id="oidc-emergency-kid"
              value={typedKid}
              onChange={(e) => setTypedKid(e.target.value)}
              placeholder={activeKey?.kid}
              autoComplete="off"
              className="font-mono text-xs"
            />
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                setEmergencyOpen(false);
                setTypedKid("");
              }}
              disabled={pending}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => rotate("emergency")}
              disabled={pending || typedKid !== activeKey?.kid}
            >
              Revoke and rotate now
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function StatusBadge({ status }: { status: KeyStatus }) {
  if (status === "active") {
    return (
      <Badge className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">
        active
      </Badge>
    );
  }
  if (status === "retired") {
    return (
      <Badge variant="secondary" title="Still verifying old tokens">
        retired — in JWKS until tokens expire
      </Badge>
    );
  }
  return <Badge variant="destructive">revoked</Badge>;
}

function fmtDate(at: string) {
  try {
    return new Date(at).toLocaleString(undefined, {
      dateStyle: "medium",
      timeStyle: "short",
    });
  } catch {
    return at;
  }
}
