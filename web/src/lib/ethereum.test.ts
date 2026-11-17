import { describe, expect, it, vi } from "vitest";
import { classifyWalletError, ensureChain, personalSign, type Eip1193Provider } from "./ethereum";

function provider(handler: (method: string, params?: unknown) => unknown): Eip1193Provider & { calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    request: vi.fn(async ({ method, params }) => {
      calls.push(method);
      return handler(method, params);
    }),
  };
}

describe("classifyWalletError", () => {
  it("recognises a rejected request", () => {
    expect(classifyWalletError({ code: 4001, message: "User rejected the request." }).kind).toBe("rejected");
  });
  it("recognises a chain missing from the wallet", () => {
    expect(classifyWalletError({ code: 4902 }).kind).toBe("unknown_chain");
    expect(classifyWalletError({ data: { originalError: { code: 4902 } } }).kind).toBe("unknown_chain");
  });
  it("keeps other messages", () => {
    const e = classifyWalletError(new Error("boom"));
    expect(e.kind).toBe("other");
    expect(e.message).toBe("boom");
  });
});

describe("ensureChain", () => {
  it("does nothing when already on the chain", async () => {
    const p = provider((m) => (m === "eth_chainId" ? "0x2105" : null));
    await ensureChain(p, 8453);
    expect(p.calls).toEqual(["eth_chainId"]);
  });

  it("asks the wallet to switch otherwise", async () => {
    let params: unknown;
    const p = provider((m, ps) => {
      if (m === "wallet_switchEthereumChain") params = ps;
      return m === "eth_chainId" ? "0x1" : null;
    });
    await ensureChain(p, 8453);
    expect(p.calls).toEqual(["eth_chainId", "wallet_switchEthereumChain"]);
    expect(params).toEqual([{ chainId: "0x2105" }]);
  });

  it("surfaces a rejected switch as a wallet error", async () => {
    const p = provider((m) => {
      if (m === "eth_chainId") return "0x1";
      throw { code: 4001, message: "User rejected" };
    });
    await expect(ensureChain(p, 137)).rejects.toMatchObject({ kind: "rejected" });
  });
});

describe("personalSign", () => {
  it("sends the UTF-8 message hex-encoded with the account", async () => {
    let params: unknown;
    const p = provider((_, ps) => {
      params = ps;
      return "0xsig";
    });
    await personalSign(p, "hi", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266");
    expect(params).toEqual(["0x6869", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"]);
  });
});
