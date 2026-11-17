import { describe, expect, it } from "vitest";
import { createSiweMessage } from "viem/siwe";
import { buildSiweMessage } from "./siwe";

const fields = {
  domain: "app.chainwatch.example",
  address: "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266", // lowercase in, checksummed out
  statement: "Sign in to chainwatch.",
  uri: "https://app.chainwatch.example",
  chainId: 8453,
  nonce: "3f9a1c0b7e2d4a6f",
  issuedAt: new Date("2026-09-29T12:00:00Z"),
  expirationTime: new Date("2026-09-29T12:10:00Z"),
};

describe("buildSiweMessage", () => {
  it("produces the exact EIP-4361 layout the server parses", () => {
    expect(buildSiweMessage(fields)).toBe(
      [
        "app.chainwatch.example wants you to sign in with your Ethereum account:",
        "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
        "",
        "Sign in to chainwatch.",
        "",
        "URI: https://app.chainwatch.example",
        "Version: 1",
        "Chain ID: 8453",
        "Nonce: 3f9a1c0b7e2d4a6f",
        "Issued At: 2026-09-29T12:00:00.000Z",
        "Expiration Time: 2026-09-29T12:10:00.000Z",
      ].join("\n"),
    );
  });

  it("matches viem's reference implementation", () => {
    const ours = buildSiweMessage(fields);
    const viem = createSiweMessage({
      domain: fields.domain,
      address: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
      statement: fields.statement,
      uri: fields.uri,
      version: "1",
      chainId: fields.chainId,
      nonce: fields.nonce,
      issuedAt: fields.issuedAt,
      expirationTime: fields.expirationTime,
    });
    expect(ours).toBe(viem);
  });

  it("omits the statement with an extra blank line, as the ABNF requires", () => {
    const msg = buildSiweMessage({ ...fields, statement: undefined, expirationTime: undefined });
    expect(msg).toContain("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266\n\n\nURI: ");
    expect(msg.endsWith("Issued At: 2026-09-29T12:00:00.000Z")).toBe(true);
  });

  it("rejects values the server would refuse", () => {
    expect(() => buildSiweMessage({ ...fields, nonce: "short" })).toThrow(/nonce/);
    expect(() => buildSiweMessage({ ...fields, statement: "two\nlines" })).toThrow(/single line/);
    expect(() => buildSiweMessage({ ...fields, address: "0x1234" })).toThrow();
  });
});
