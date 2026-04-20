"use client";

import type { ReactNode } from "react";
import { ThemeProvider as NextThemesProvider } from "next-themes";

// ThemeProvider wraps next-themes so our own ThemeToggle can flip
// the `.dark` class on <html>. Attribute="class" is the Tailwind
// v4 expectation; defaultTheme=system means a first-visit user
// matches their OS preference.
export function ThemeProvider({ children }: { children: ReactNode }) {
  return (
    <NextThemesProvider
      attribute="class"
      defaultTheme="system"
      enableSystem
      disableTransitionOnChange
    >
      {children}
    </NextThemesProvider>
  );
}
