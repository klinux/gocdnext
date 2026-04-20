import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

// Next.js middleware runs at the edge before any RSC render. It
// has two jobs in the auth flow:
//
//   1. Short-circuit anonymous requests on dashboard routes: if
//      there's no session cookie, redirect immediately to /login
//      with the original path as ?next=. Saves us from rendering
//      a layout that will bounce anyway.
//
//   2. Forward the current pathname as an x-pathname header so
//      the (dashboard) layout can build a proper ?next= when it
//      decides (post-DB lookup) that the cookie is invalid.
//
// The cookie check here is presence-only — a valid cookie that's
// actually expired still gets caught by the layout's DB-backed
// resolveAuthState(). That's the safety net; this middleware is
// just the fast path.

const SESSION_COOKIE = "gocdnext_session";

export function middleware(req: NextRequest) {
  const { pathname, search } = req.nextUrl;
  const fullPath = pathname + search;

  const hasSession = req.cookies.has(SESSION_COOKIE);
  if (!hasSession) {
    const loginURL = new URL("/login", req.nextUrl);
    loginURL.searchParams.set("next", fullPath);
    return NextResponse.redirect(loginURL);
  }

  const headers = new Headers(req.headers);
  headers.set("x-pathname", fullPath);
  return NextResponse.next({ request: { headers } });
}

// Matcher excludes:
//   - /login (anonymous by definition)
//   - /_next/*, static assets, favicon (no point doing auth here)
//   - Next's own /api if we ever add route handlers that bypass
//     the control plane
export const config = {
  matcher: ["/((?!login|_next|api/.*|favicon.ico|.*\\.(?:svg|png|jpg|jpeg|webp|gif|ico)).*)"],
};
