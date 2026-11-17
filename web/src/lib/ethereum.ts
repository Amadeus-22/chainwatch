import { getAddress, numberToHex, toHex, type Address, type Hex } from "viem";
import type { TxRequest } from "./revoke";

// Minimal EIP-1193 access to the injected wallet (MetaMask). No wallet SDK:
// window.ethereum is the whole integration surface.

export interface Eip1193Provider {
  request(args: { method: string; params?: unknown[] | Record<string, unknown> }): Promise<unknown>;
  on?(event: string, handler: (...args: unknown[]) => void): void;
  removeListener?(event: string, handler: (...args: unknown[]) => void): void;
  isMetaMask?: boolean;
}

declare global {
  interface Window {
    ethereum?: Eip1193Provider;
  }
}

export function getProvider(): Eip1193Provider | undefined {
  return typeof window === "undefined" ? undefined : window.ethereum;
}

export type WalletErrorKind = "rejected" | "unknown_chain" | "pending" | "other";

/** A wallet failure the UI can explain. */
export class WalletError extends Error {
  constructor(
    public readonly kind: WalletErrorKind,
    message: string,
  ) {
    super(message);
    this.name = "WalletError";
  }
}

/** Maps EIP-1193 / MetaMask error codes to what the user should be told. */
export function classifyWalletError(err: unknown): WalletError {
  if (err instanceof WalletError) {
    return err;
  }
  const e = err as { code?: number; message?: string; data?: { originalError?: { code?: number } } } | undefined;
  const code = e?.code ?? e?.data?.originalError?.code;
  switch (code) {
    case 4001:
      return new WalletError("rejected", "You rejected the request in your wallet.");
    case 4902:
      return new WalletError("unknown_chain", "This network is not added to your wallet. Add it in MetaMask, then try again.");
    case -32002:
      return new WalletError("pending", "A request is already pending in your wallet. Open MetaMask to continue.");
    default:
      return new WalletError("other", e?.message ?? "The wallet request failed.");
  }
}

async function call<T>(p: Eip1193Provider, method: string, params?: unknown[]): Promise<T> {
  try {
    return (await p.request({ method, params })) as T;
  } catch (err) {
    throw classifyWalletError(err);
  }
}

export async function requestAccounts(p: Eip1193Provider): Promise<Address[]> {
  const accounts = await call<string[]>(p, "eth_requestAccounts");
  return accounts.map((a) => getAddress(a));
}

export async function connectedAccounts(p: Eip1193Provider): Promise<Address[]> {
  const accounts = await call<string[]>(p, "eth_accounts");
  return accounts.map((a) => getAddress(a));
}

export async function currentChainId(p: Eip1193Provider): Promise<number> {
  return Number.parseInt(await call<string>(p, "eth_chainId"), 16);
}

export async function switchChain(p: Eip1193Provider, chainId: number): Promise<void> {
  await call(p, "wallet_switchEthereumChain", [{ chainId: numberToHex(chainId) }]);
}

/** Switches the wallet to chainId unless it is already there. */
export async function ensureChain(p: Eip1193Provider, chainId: number): Promise<void> {
  if ((await currentChainId(p)) !== chainId) {
    await switchChain(p, chainId);
  }
}

/** personal_sign (EIP-191) of a UTF-8 message; returns the 65-byte signature. */
export async function personalSign(p: Eip1193Provider, message: string, account: Address): Promise<Hex> {
  return call<Hex>(p, "personal_sign", [toHex(message), account]);
}

export async function sendTransaction(p: Eip1193Provider, tx: TxRequest): Promise<Hex> {
  return call<Hex>(p, "eth_sendTransaction", [tx]);
}
