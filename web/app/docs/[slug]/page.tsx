import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { DocRenderer } from "@/components/docs/doc-renderer";
import { readDoc, listDocs } from "@/server/queries/docs";

type Params = { slug: string };

// generateStaticParams pre-renders every doc at build time so a
// fresh docs visit is a single static response (no fs read on
// the hot path). When new files land under monorepo docs/, a
// rebuild picks them up.
export async function generateStaticParams() {
  const docs = await listDocs();
  return docs.map((d) => ({ slug: d.slug }));
}

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
