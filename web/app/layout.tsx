import type { Metadata } from "next";
import type { ReactNode } from "react";
import { Geist } from "next/font/google";
import { cn } from "@/lib/utils";
import { ThemeProvider } from "@/components/providers/theme-provider.client";
import { TooltipProvider } from "@/components/ui/tooltip";
import "./globals.css";

const geist = Geist({ subsets: ["latin"], variable: "--font-sans" });

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
    <html lang="en" suppressHydrationWarning className={cn("font-sans", geist.variable)}>
      <body className="bg-background text-foreground min-h-screen antialiased">
        <ThemeProvider>
          <TooltipProvider>{children}</TooltipProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
