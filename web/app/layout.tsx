import type { Metadata } from "next";
import type { ReactNode } from "react";
import { Geist } from "next/font/google";
import { cn } from "@/lib/utils";
import "./globals.css";

const geist = Geist({ subsets: ["latin"], variable: "--font-sans" });

export const metadata: Metadata = {
  title: "gocdnext",
  description: "Modern CI/CD orchestrator",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning className={cn("font-sans", geist.variable)}>
      <body className="bg-background text-foreground min-h-screen antialiased">
        {children}
      </body>
    </html>
  );
}
