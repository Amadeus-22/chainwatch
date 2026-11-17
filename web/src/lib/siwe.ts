import { getAddress } from "viem";

// Sign-In with Ethereum (EIP-4361) message builder. The server parses exactly
// this layout (internal/domain/siwe), so keep the two in step.

export interface SiweFields {
  /** host[:port] of this app, e.g. "app.example.com" or "localhost:5173". */
  domain: string;
  address: string;
  statement?: string;
  /** Absolute URI of this app, e.g. "https://app.example.com". */
  uri: string;
  chainId: number;
  nonce: string;
  issuedAt: Date;
  expirationTime?: Date;
}

export function buildSiweMessage(f: SiweFields): string {
  if (f.statement?.includes("\n")) {
    throw new Error("statement must be a single line");
  }
  if (!/^[a-zA-Z0-9]{8,}$/.test(f.nonce)) {
    throw new Error("nonce must be at least 8 alphanumeric characters");
  }
  const lines = [
    `${f.domain} wants you to sign in with your Ethereum account:`,
    getAddress(f.address), // EIP-55 checksum, required by EIP-4361
    "",
  ];
  if (f.statement) {
    lines.push(f.statement);
  }
  lines.push(
    "",
    `URI: ${f.uri}`,
    "Version: 1",
    `Chain ID: ${f.chainId}`,
    `Nonce: ${f.nonce}`,
    `Issued At: ${f.issuedAt.toISOString()}`,
  );
  if (f.expirationTime) {
    lines.push(`Expiration Time: ${f.expirationTime.toISOString()}`);
  }
  return lines.join("\n");
}
