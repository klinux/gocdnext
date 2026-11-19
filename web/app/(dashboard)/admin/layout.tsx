import type { ReactNode } from "react";

import { requireAdmin } from "@/server/queries/access";

// Every /admin/* read is admin-only server-side. The sidebar hides these
// links from non-admins; this layout check is a fast redirect on a direct hit.
// It is NOT the barrier: Next renders layout and page in parallel and reuses
// layouts across client navigations, so each page also calls requireAdmin()
// as its first statement (enforced by privileged-pages.test.ts).
export default async function AdminLayout({
  children,
}: {
  children: ReactNode;
}) {
  await requireAdmin();
  return children;
}
