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
  return (
    <div className="space-y-6 px-4 py-6 md:px-8">
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
