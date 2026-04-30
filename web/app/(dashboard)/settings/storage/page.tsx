import type { Metadata } from "next";

import { StorageForm } from "@/components/settings/storage-form.client";
import { getStorageConfig } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Storage",
};

// force-dynamic so a save in this tab is reflected on the next view
// without leaning on the data cache. The payload is a single small
// row, so no perf concern.
export const dynamic = "force-dynamic";

export default async function StorageSettingsPage() {
  const config = await getStorageConfig();
  return <StorageForm initial={config} />;
}
