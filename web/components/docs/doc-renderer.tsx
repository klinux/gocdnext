import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";

// GitHub Dark Dimmed matches the rest of the app's muted dark
// surfaces — the docs tab rendered against the `bg-muted/40`
// pre chrome reads as consistent with status/log viewers
// elsewhere. highlight.js emits plain .hljs-* classnames so a
// single theme stylesheet covers every code block.
import "highlight.js/styles/github-dark-dimmed.css";

import { cn } from "@/lib/utils";

type Props = {
  markdown: string;
  className?: string;
};

// DocRenderer turns a markdown string into the page body. Kept
// as a plain (non-client) component so the rendered HTML ships
// in the initial payload — no hydration step for a static doc.
// Syntax highlighting runs server-side via rehype-highlight so
// we don't ship highlight.js to the browser.
//
// Tailwind prose classes give the default typographic rhythm;
// a handful of overrides tighten pre/code tones so they match
// the app's monospaced UI blocks elsewhere.
export function DocRenderer({ markdown, className }: Props) {
  return (
    <article
      className={cn(
        "prose prose-slate max-w-none dark:prose-invert",
        "prose-headings:scroll-mt-20 prose-headings:tracking-tight",
        "prose-code:rounded prose-code:bg-muted prose-code:px-1 prose-code:py-0.5 prose-code:text-[0.9em] prose-code:font-normal prose-code:before:content-none prose-code:after:content-none",
        "prose-pre:rounded-md prose-pre:border prose-pre:border-border prose-pre:bg-muted/40 prose-pre:p-4",
        "prose-a:text-primary prose-a:no-underline hover:prose-a:underline",
        "prose-hr:border-border",
        "prose-blockquote:border-l-border prose-blockquote:text-muted-foreground",
        className,
      )}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: true, ignoreMissing: true }]]}
      >
        {markdown}
      </ReactMarkdown>
    </article>
  );
}
