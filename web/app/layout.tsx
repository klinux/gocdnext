import type { Metadata } from "next";
import type { ReactNode } from "react";
import { Inter, JetBrains_Mono } from "next/font/google";
import { cn } from "@/lib/utils";
import { ThemeProvider } from "@/components/providers/theme-provider.client";
import { TooltipProvider } from "@/components/ui/tooltip";
import "./globals.css";

// Canonical type pairing (design handoff): Inter for UI text, JetBrains Mono
// for keys / numeric cells / tier chips / code-like labels.
const inter = Inter({ subsets: ["latin"], variable: "--font-sans" });
const jetbrainsMono = JetBrains_Mono({ subsets: ["latin"], variable: "--font-mono" });

export const metadata: Metadata = {
  title: {
    default: "gocdnext",
    template: "%s · gocdnext",
  },
  description:
    "Modern CI/CD orchestrator — stages, jobs, secrets, artifacts, on your own agents.",
  applicationName: "gocdnext",
  openGraph: {
    title: "gocdnext",
    description: "Modern CI/CD orchestrator",
    siteName: "gocdnext",
    type: "website",
  },
  twitter: {
    card: "summary_large_image",
    title: "gocdnext",
    description: "Modern CI/CD orchestrator",
  },
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html
      lang="en"
      suppressHydrationWarning
      className={cn("font-sans", inter.variable, jetbrainsMono.variable)}
    >
      <body className="bg-background text-foreground min-h-screen antialiased">
        <ThemeProvider>
          <TooltipProvider>{children}</TooltipProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
