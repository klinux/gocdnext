// RSC route guards for privileged sections. The nav already hides links a
// user can't use; these cover the other ways in (typed URL, bookmark, a link
// pasted in chat, a role demoted while a tab was open) by sending the user
// somewhere they CAN see instead of rendering a page whose reads would 403.
// Call them as the FIRST statement of the page (before any read), not only in
// a layout — layouts render in parallel with the page and are reused across
// client navigations. The server's RequireMinRole stays the enforcement point.

import type { Route } from "next";
import { redirect } from "next/navigation";

import { isAdmin, isMaintainer, resolveAuthState } from "./auth";

export async function requireAdmin(fallback: Route = "/"): Promise<void> {
  if (!isAdmin(await resolveAuthState())) redirect(fallback);
}

export async function requireMaintainer(fallback: Route = "/"): Promise<void> {
  if (!isMaintainer(await resolveAuthState())) redirect(fallback);
}
