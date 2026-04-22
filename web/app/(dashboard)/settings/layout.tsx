import type { Metadata } from "next";
import type { ReactNode } from "react";

import { SettingsTabs } from "@/components/settings/settings-tabs.client";

export const metadata: Metadata = {
  title: "Settings — gocdnext",
};

export default function SettingsLayout({ children }: { children: ReactNode }) {
  // Dashboard layout already pads the content area — we'd be
  // doubling horizontal padding by re-applying it here, which
  // is what made /settings look narrower than every other page.
  return (
    <div className="space-y-6">
      <header className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
        <p className="text-sm text-muted-foreground">
          Control-plane health, webhook audit trail, retention policy and
          provider integrations.
        </p>
      </header>
      <div className="flex flex-col gap-6 md:flex-row md:items-start">
        <SettingsTabs />
        <div className="min-w-0 flex-1">{children}</div>
      </div>
    </div>
  );
}
