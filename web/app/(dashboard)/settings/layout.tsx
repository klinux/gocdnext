import type { Metadata, Route } from "next";
import type { ReactNode } from "react";
import {
  Activity,
  KeyRound,
  Lock,
  Plug,
  Archive,
  Webhook,
} from "lucide-react";

import {
  SettingsTabs,
  type SettingsTab,
} from "@/components/settings/settings-tabs.client";

export const metadata: Metadata = {
  title: "Settings — gocdnext",
};

const tabs: SettingsTab[] = [
  { label: "Health", href: "/settings/health" as Route, matchPrefix: "/settings/health", icon: Activity },
  { label: "Webhooks", href: "/settings/webhooks" as Route, matchPrefix: "/settings/webhooks", icon: Webhook },
  { label: "Retention", href: "/settings/retention" as Route, matchPrefix: "/settings/retention", icon: Archive },
  { label: "Integrations", href: "/settings/integrations" as Route, matchPrefix: "/settings/integrations", icon: Plug },
  { label: "Auth", href: "/settings/auth" as Route, matchPrefix: "/settings/auth", icon: Lock },
  { label: "Secrets", href: "/settings/secrets" as Route, matchPrefix: "/settings/secrets", icon: KeyRound },
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
      <div className="flex flex-col gap-6 md:flex-row md:items-start">
        <SettingsTabs tabs={tabs} />
        <div className="min-w-0 flex-1">{children}</div>
      </div>
    </div>
  );
}
