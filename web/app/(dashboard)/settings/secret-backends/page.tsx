import type { Metadata } from "next";

import { SecretBackendsForm } from "@/components/settings/secret-backends-form.client";
import { listSecretBackends } from "@/server/queries/admin";
import { requireAdmin } from "@/server/queries/access";

export const metadata: Metadata = {
  title: "Settings — Secret backends",
};

// force-dynamic so a save in this tab is reflected on the next view
// without leaning on the data cache. The payload is three small rows,
// so no perf concern.
export const dynamic = "force-dynamic";

export default async function SecretBackendsSettingsPage() {
  await requireAdmin();
  const backends = await listSecretBackends();
  return <SecretBackendsForm initial={backends} />;
}
