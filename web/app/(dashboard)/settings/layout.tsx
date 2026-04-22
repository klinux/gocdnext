import type { Metadata, Route } from "next";
import type { ReactNode } from "react";

import { SettingsTabs } from "@/components/settings/settings-tabs.client";

export const metadata: Metadata = {
  title: "Settings — gocdnext",
};

type Tab = { label: string; href: Route; matchPrefix: string };

const tabs: Tab[] = [
  { label: "Health", href: "/settings/health" as Route, matchPrefix: "/settings/health" },
  { label: "Webhooks", href: "/settings/webhooks" as Route, matchPrefix: "/settings/webhooks" },
  { label: "Retention", href: "/settings/retention" as Route, matchPrefix: "/settings/retention" },
  { label: "Integrations", href: "/settings/integrations" as Route, matchPrefix: "/settings/integrations" },
  { label: "Auth", href: "/settings/auth" as Route, matchPrefix: "/settings/auth" },
  { label: "Secrets", href: "/settings/secrets" as Route, matchPrefix: "/settings/secrets" },
];

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
      <SettingsTabs tabs={tabs} />
      <div>{children}</div>
    </div>
  );
}
