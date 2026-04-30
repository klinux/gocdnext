import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { DocRenderer } from "@/components/docs/doc-renderer";
import { readDoc } from "@/server/queries/docs";

type Params = { slug: string };

// force-dynamic so the doc reads land at request time. Static
// prerendering would run at build time when docs/ isn't yet at
// the runtime path (the standalone container mounts docs/ as a
// sibling of /app — only visible at request time).
export const dynamic = "force-dynamic";

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  const doc = await readDoc(slug);
  if (!doc) return { title: "Not found — gocdnext docs" };
  return { title: `${doc.title} — gocdnext docs` };
}

export default async function DocPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;
  const doc = await readDoc(slug);
  if (!doc) notFound();

  return <DocRenderer markdown={doc.markdown} />;
}
