import type { Metadata } from "next";
import { redirect } from "next/navigation";

import { LocalLoginForm } from "@/components/auth/local-login-form.client";
import { Logo } from "@/components/brand/logo";
import { env } from "@/lib/env";
import { listProviders } from "@/server/queries/auth";

export const metadata: Metadata = {
  title: "Sign in — gocdnext",
};

export const dynamic = "force-dynamic";

type Props = {
  searchParams: Promise<{ next?: string | string[]; error?: string | string[] }>;
};

export default async function LoginPage({ searchParams }: Props) {
  const providers = await listProviders();
  if (!providers.enabled) {
    // Dev deployments leave auth off; send visitors to the dashboard
    // instead of showing an empty login page.
    redirect("/");
  }

  const sp = await searchParams;
  const next = strParam(sp.next);
  const error = strParam(sp.error);

  const loginBase = env.GOCDNEXT_API_URL.replace(/\/+$/, "");
  const sanitizedNext = sanitizeNext(next);

  return (
    <main className="flex min-h-svh items-center justify-center bg-muted/30 px-4">
      <div className="w-full max-w-sm space-y-6 rounded-xl border bg-background p-8 shadow-sm">
        <header className="flex flex-col items-center text-center">
          <Logo size={40} className="mb-3 text-foreground" />
          <h1 className="text-lg font-semibold tracking-tight">
            Sign in to gocdnext
          </h1>
          <p className="mt-1 text-xs text-muted-foreground">
            Choose your identity provider.
          </p>
        </header>

        {error ? (
          <div
            role="alert"
            className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive"
          >
            {prettifyError(error)}
          </div>
        ) : null}

        {providers.providers.length === 0 && !providers.local_enabled ? (
          <p className="rounded-md border bg-muted/50 px-3 py-2 text-xs text-muted-foreground">
            No identity providers configured. Either set{" "}
            <code className="font-mono">GOCDNEXT_AUTH_*</code> env vars +
            restart, or seed a local admin with{" "}
            <code className="font-mono">gocdnext admin create-user</code>.
          </p>
        ) : null}

        {providers.providers.length > 0 ? (
          <ul className="space-y-2">
            {providers.providers.map((p) => {
              const qs = sanitizedNext
                ? `?next=${encodeURIComponent(sanitizedNext)}`
                : "";
              return (
                <li key={p.name}>
                  <a
                    href={`${loginBase}/auth/login/${p.name}${qs}`}
                    className="flex w-full items-center justify-center gap-2 rounded-md border bg-background px-3 py-2 text-sm font-medium transition-colors hover:bg-muted"
                  >
                    Continue with {p.display}
                  </a>
                </li>
              );
            })}
          </ul>
        ) : null}

        {providers.local_enabled && providers.providers.length > 0 ? (
          <div className="flex items-center gap-3">
            <div className="h-px flex-1 bg-border" />
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
              or
            </span>
            <div className="h-px flex-1 bg-border" />
          </div>
        ) : null}

        {providers.local_enabled ? (
          <LocalLoginForm next={sanitizedNext || "/"} />
        ) : null}

        <p className="text-center text-[10px] uppercase tracking-wide text-muted-foreground/70">
          control plane
        </p>
      </div>
    </main>
  );
}

function strParam(v: string | string[] | undefined): string {
  if (Array.isArray(v)) return v[0] ?? "";
  return v ?? "";
}

// sanitizeNext mirrors the server-side check: only same-origin paths
// survive. Absolute URLs or protocol-relative //evil.com get dropped.
function sanitizeNext(v: string): string {
  if (!v || !v.startsWith("/") || v.startsWith("//")) return "";
  try {
    const u = new URL(v, "http://local");
    if (u.host !== "local") return "";
    return v;
  } catch {
    return "";
  }
}

function prettifyError(raw: string): string {
  switch (raw) {
    case "state":
      return "Sign-in expired. Please try again.";
    case "forbidden":
      return "Your email is not allowed to sign in.";
    case "provider":
      return "The identity provider rejected the sign-in.";
    default:
      return raw;
  }
}
