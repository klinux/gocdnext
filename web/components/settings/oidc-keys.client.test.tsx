import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { OIDCKeysManager } from "./oidc-keys.client";
import type { OIDCKey } from "@/types/api";

// Server action mocked at module level — rotation must never fire a
// real fetch from a unit test.
vi.mock("@/server/actions/oidc-keys", () => ({
  rotateOIDCKey: vi.fn(async () => ({
    ok: true,
    data: { kid: "new-kid", mode: "graceful", note: "" },
  })),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

const refresh = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh }),
}));

import { rotateOIDCKey } from "@/server/actions/oidc-keys";

const active: OIDCKey = {
  id: "k1",
  kid: "ooa_QM5t-active",
  alg: "RS256",
  created_at: "2026-06-01T10:00:00Z",
};

const retired: OIDCKey = {
  id: "k2",
  kid: "old-retired-kid",
  alg: "RS256",
  created_at: "2026-05-01T10:00:00Z",
  retired_at: "2026-06-01T10:00:00Z",
};

const revoked: OIDCKey = {
  id: "k3",
  kid: "bad-revoked-kid",
  alg: "RS256",
  created_at: "2026-04-01T10:00:00Z",
  revoked_at: "2026-05-01T10:00:00Z",
};

beforeEach(() => {
  vi.mocked(rotateOIDCKey).mockClear();
  refresh.mockClear();
});

describe("OIDCKeysManager", () => {
  it("renders one row per key with its lifecycle status", () => {
    render(<OIDCKeysManager keys={[active, retired, revoked]} />);
    expect(screen.getByText("ooa_QM5t-active")).toBeTruthy();
    expect(screen.getByText("old-retired-kid")).toBeTruthy();
    expect(screen.getByText("bad-revoked-kid")).toBeTruthy();
    expect(screen.getByText(/^active$/i)).toBeTruthy();
    expect(screen.getByText(/in JWKS until tokens expire/i)).toBeTruthy();
    expect(screen.getByText(/^revoked$/i)).toBeTruthy();
  });

  it("shows the disabled-issuer hint when there are no keys", () => {
    render(<OIDCKeysManager keys={[]} />);
    expect(screen.getByText(/issuer has never generated a key/i)).toBeTruthy();
  });

  it("graceful rotate asks confirm() and calls the action", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(<OIDCKeysManager keys={[active]} />);
    fireEvent.click(screen.getByRole("button", { name: /rotate key/i }));
    expect(confirmSpy).toHaveBeenCalled();
    await waitFor(() =>
      expect(rotateOIDCKey).toHaveBeenCalledWith({ mode: "graceful" }),
    );
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    confirmSpy.mockRestore();
  });

  it("graceful rotate aborts when confirm() is declined", () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<OIDCKeysManager keys={[active]} />);
    fireEvent.click(screen.getByRole("button", { name: /rotate key/i }));
    expect(rotateOIDCKey).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });

  it("emergency rotate stays disabled until the active kid is typed", async () => {
    render(<OIDCKeysManager keys={[active, retired]} />);
    fireEvent.click(screen.getByRole("button", { name: /emergency rotate/i }));

    // Dialog open: the confirm button starts disabled.
    const confirmBtn = await screen.findByRole("button", {
      name: /revoke and rotate now/i,
    });
    expect((confirmBtn as HTMLButtonElement).disabled).toBe(true);

    const input = screen.getByLabelText(/type the active kid/i);
    fireEvent.change(input, { target: { value: "wrong-kid" } });
    expect((confirmBtn as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(input, { target: { value: "ooa_QM5t-active" } });
    expect((confirmBtn as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(confirmBtn);
    await waitFor(() =>
      expect(rotateOIDCKey).toHaveBeenCalledWith({ mode: "emergency" }),
    );
  });

  it("hides the emergency button when no key is active", () => {
    render(<OIDCKeysManager keys={[revoked]} />);
    expect(
      screen.queryByRole("button", { name: /emergency rotate/i }),
    ).toBeNull();
  });
});
