"use client";

import { useEffect, useState } from "react";
import type { Route } from "next";
import { useRouter } from "next/navigation";
import { useTheme } from "next-themes";
import {
  Activity,
  Boxes,
  LayoutDashboard,
  Monitor,
  Moon,
  Search,
  Server,
  Settings,
  Sun,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
  CommandShortcut,
} from "@/components/ui/command";

// CommandPalette: always-mounted dialog bound to ⌘K / Ctrl+K. The
// trigger button on the topbar doubles as a visual hint. Keeping
// the state here means the shortcut works globally without a
// context or zustand store.
export function CommandPalette() {
  const router = useRouter();
  const { setTheme } = useTheme();
  const [open, setOpen] = useState(false);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

  const go = (href: Route) => {
    setOpen(false);
    router.push(href);
  };

  return (
    <>
      <Button
        variant="outline"
        size="sm"
        className="h-8 gap-2 text-xs text-muted-foreground"
        onClick={() => setOpen(true)}
      >
        <Search className="size-3.5" />
        <span className="hidden sm:inline">Search…</span>
        <CommandShortcut className="hidden sm:inline">⌘K</CommandShortcut>
      </Button>
      <CommandDialog open={open} onOpenChange={setOpen} title="Command" description="Jump to a page or run an action.">
        <CommandInput placeholder="Type a command or search…" />
        <CommandList>
          <CommandEmpty>No results.</CommandEmpty>
          <CommandGroup heading="Go to">
            <CommandItem onSelect={() => go("/" as Route)}>
              <LayoutDashboard className="size-4" />
              Dashboard
            </CommandItem>
            <CommandItem onSelect={() => go("/projects" as Route)}>
              <Boxes className="size-4" />
              Projects
            </CommandItem>
            <CommandItem disabled>
              <Activity className="size-4" />
              Runs
              <CommandShortcut>soon</CommandShortcut>
            </CommandItem>
            <CommandItem disabled>
              <Server className="size-4" />
              Agents
              <CommandShortcut>soon</CommandShortcut>
            </CommandItem>
            <CommandItem disabled>
              <Settings className="size-4" />
              Settings
              <CommandShortcut>soon</CommandShortcut>
            </CommandItem>
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Theme">
            <CommandItem
              onSelect={() => {
                setTheme("light");
                setOpen(false);
              }}
            >
              <Sun className="size-4" />
              Light
            </CommandItem>
            <CommandItem
              onSelect={() => {
                setTheme("dark");
                setOpen(false);
              }}
            >
              <Moon className="size-4" />
              Dark
            </CommandItem>
            <CommandItem
              onSelect={() => {
                setTheme("system");
                setOpen(false);
              }}
            >
              <Monitor className="size-4" />
              System
            </CommandItem>
          </CommandGroup>
        </CommandList>
      </CommandDialog>
    </>
  );
}
