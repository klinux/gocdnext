"use client";

import { useState } from "react";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";

import { FrameworksManager } from "./frameworks-manager.client";
import { PoliciesManager } from "./policies-manager.client";
import type { PreviewProject } from "./use-policy-preview";

export function ComplianceManager({
  frameworks: initialFrameworks,
  policies: initialPolicies,
  projects,
}: {
  frameworks: ComplianceFramework[];
  policies: CompliancePolicy[];
  projects: PreviewProject[];
}) {
  // Frameworks state is lifted so a framework created in the Frameworks tab is
  // immediately selectable as a policy target in the Policies tab (and the tab
  // counts stay live) without a page refresh.
  const [frameworks, setFrameworks] = useState(initialFrameworks);
  const [policies, setPolicies] = useState(initialPolicies);

  return (
    <Tabs defaultValue="policies" className="space-y-4">
      <TabsList>
        <TabsTrigger value="policies">Policies ({policies.length})</TabsTrigger>
        <TabsTrigger value="frameworks">
          Frameworks ({frameworks.length})
        </TabsTrigger>
      </TabsList>
      <TabsContent value="policies">
        <PoliciesManager
          policies={policies}
          setPolicies={setPolicies}
          frameworks={frameworks}
          projects={projects}
        />
      </TabsContent>
      <TabsContent value="frameworks">
        <FrameworksManager frameworks={frameworks} setFrameworks={setFrameworks} />
      </TabsContent>
    </Tabs>
  );
}
