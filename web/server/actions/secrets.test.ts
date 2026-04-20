import { describe, expect, it } from "vitest";
import { secretNameSchema } from "@/lib/validations";

describe("secretNameSchema", () => {
  it("accepts valid env-var-shaped names", () => {
    const cases = ["GH_TOKEN", "a", "My_Secret_42", "slack_webhook"];
    for (const c of cases) {
      expect(secretNameSchema.safeParse(c).success).toBe(true);
    }
  });

  it("rejects empty, leading-digit, and punctuation", () => {
    const cases = ["", "1TOKEN", "token-with-dash", "has space", "sep.chars"];
    for (const c of cases) {
      expect(secretNameSchema.safeParse(c).success).toBe(false);
    }
  });

  it("caps length at 64", () => {
    const ok = "A".repeat(64);
    const tooLong = "A".repeat(65);
    expect(secretNameSchema.safeParse(ok).success).toBe(true);
    expect(secretNameSchema.safeParse(tooLong).success).toBe(false);
  });
});
