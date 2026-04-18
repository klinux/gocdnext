"use client";

import {
  QueryClient,
  QueryClientProvider as TanstackProvider,
} from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

// One QueryClient per user session: `useState` keeps the same instance across
// renders but gives each SSR-hydrated session its own cache, which is the
// pattern TanStack recommends for RSC apps.
export function QueryClientProvider({ children }: { children: ReactNode }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            // Don't refetch while the tab is backgrounded — polling-intensive
            // dashboards waste bandwidth otherwise. Live queries explicitly
            // set their own refetchInterval.
            refetchOnWindowFocus: false,
            retry: 1,
          },
        },
      }),
  );
  return <TanstackProvider client={client}>{children}</TanstackProvider>;
}
