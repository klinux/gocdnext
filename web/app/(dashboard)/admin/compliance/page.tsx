import type { Metadata } from "next";

import { ComplianceManager } from "@/components/compliance/compliance-manager.client";
import {
  getCompliancePolicy,
  listComplianceFrameworks,
  listCompliancePolicies,
} from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Compliance — gocdnext",
};

// Mutations revalidate via the action; force-dynamic keeps multi-tab edits in
// sync. Payload is small (a handful of frameworks/policies).
export const dynamic = "force-dynamic";

export default async function CompliancePage() {
  const [frameworks, policyList] = await Promise.all([
    listComplianceFrameworks(),
    listCompliancePolicies(),
  ]);
  // The list endpoint omits config_yaml + framework_ids (metadata only); fetch
  // each full policy so the editor opens fully populated. Few policies in
  // practice, so the N extra reads are cheap.
  const policies = await Promise.all(
    policyList.map((p) => getCompliancePolicy(p.id)),
  );

  return (
    <section className="space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Compliance</h2>
        <p className="text-sm text-muted-foreground">
          Framework-scoped, enforced pipeline policies. A policy&apos;s mandatory
          jobs / approval gates are merged into every targeted project and can&apos;t
          be removed or bypassed from the project&apos;s repo.
        </p>
      </div>

      <ComplianceManager frameworks={frameworks} policies={policies} />
    </section>
  );
}
