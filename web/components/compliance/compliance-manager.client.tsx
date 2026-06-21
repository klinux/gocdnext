"use client";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";

import { FrameworksManager } from "./frameworks-manager.client";
import { PoliciesManager } from "./policies-manager.client";

export function ComplianceManager({
  frameworks,
  policies,
}: {
  frameworks: ComplianceFramework[];
  policies: CompliancePolicy[];
}) {
  return (
    <Tabs defaultValue="policies" className="space-y-4">
      <TabsList>
        <TabsTrigger value="policies">Policies ({policies.length})</TabsTrigger>
        <TabsTrigger value="frameworks">
          Frameworks ({frameworks.length})
        </TabsTrigger>
      </TabsList>
      <TabsContent value="policies">
        <PoliciesManager policies={policies} frameworks={frameworks} />
      </TabsContent>
      <TabsContent value="frameworks">
        <FrameworksManager frameworks={frameworks} />
      </TabsContent>
    </Tabs>
  );
}
