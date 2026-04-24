import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

// Next.js proxy (renamed from "middleware" in Next 16) runs at
// the edge before any RSC render. Its only job is to forward the
// request's path+query to the layout via an x-pathname header so
// the layout can build a correct ?next=<path> when it decides
// (post-DB lookup) that the session is invalid.
//
// We deliberately do NOT redirect here based on cookie presence:
// the control plane may have auth turned off (the dev default),
// in which case a "no cookie → /login" rule loops forever
// (/login sees auth disabled and bounces back to /). The layout
// is the only place that knows the real auth state, so gate
// decisions live there.

export function proxy(req: NextRequest) {
  const { pathname, search } = req.nextUrl;
  const headers = new Headers(req.headers);
  headers.set("x-pathname", pathname + search);
  return NextResponse.next({ request: { headers } });
}

// Matcher excludes only the assets and webhook-style paths that
// don't care about the pathname header. /login is kept in so the
// header is still forwarded — the login page itself uses it to
// survive a refresh.
export const config = {
  matcher: ["/((?!_next|api/.*|favicon.ico|.*\\.(?:svg|png|jpg|jpeg|webp|gif|ico)).*)"],
};
