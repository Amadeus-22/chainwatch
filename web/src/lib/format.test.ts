import { describe, expect, it } from "vitest";
import { explorerLink, formatBaseUnits, readableReason, sameAddress, shortAddress, timeAgo } from "./format";

describe("format", () => {
  it("shortens checksummed addresses", () => {
    expect(shortAddress("0x70997970c51812dc3a010c7d01b50e0d17dc79c8")).toBe("0x7099…79C8");
  });
  it("groups digits of uint256-sized amounts without losing precision", () => {
    expect(formatBaseUnits("1000000000000000000000")).toBe("1,000,000,000,000,000,000,000");
    expect(formatBaseUnits("5")).toBe("5");
  });
  it("compares addresses case-insensitively", () => {
    expect(sameAddress("0xAbC", "0xabc")).toBe(true);
    expect(sameAddress(undefined, "0xabc")).toBe(false);
  });
  it("builds explorer links only when an explorer is configured", () => {
    expect(explorerLink("https://basescan.org/", "tx", "0x12")).toBe("https://basescan.org/tx/0x12");
    expect(explorerLink(undefined, "tx", "0x12")).toBeUndefined();
  });
  it("renders relative times", () => {
    const now = new Date("2026-09-29T12:00:00Z");
    expect(timeAgo("2026-09-29T11:59:30Z", now)).toBe("30s ago");
    expect(timeAgo("2026-09-29T09:00:00Z", now)).toBe("3h ago");
  });
  it("makes alert reasons readable", () => {
    const max = (2n ** 256n - 1n).toString();
    expect(
      readableReason(
        `wallet 0x70997970c51812dc3a010c7d01b50e0d17dc79c8 approved ${max} of token 0x5fbdb2315678afecb367f032d93f642f64180aa3 at block 16 (tx 0xb439ec84fb7de6266ba2f8bd2e73c9c21d7c02c1711438819d23ffb643fef1ef)`,
      ),
    ).toBe("wallet 0x7099…79C8 approved unlimited of token 0x5FbD…0aa3 at block 16 (tx 0xb439ec84…)");
    expect(readableReason("100000000000000000000 left")).toBe("100,000,000,000,000,000,000 left");
  });
});
