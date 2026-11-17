import { getAddress } from "viem";

export function checksum(address: string): string {
  try {
    return getAddress(address);
  } catch {
    return address;
  }
}

export function shortAddress(address: string): string {
  const a = checksum(address);
  return `${a.slice(0, 6)}…${a.slice(-4)}`;
}

/** Groups the digits of a base-unit amount (a decimal string of any size). */
export function formatBaseUnits(amount: string): string {
  return amount.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

export function sameAddress(a: string | undefined, b: string | undefined): boolean {
  return !!a && !!b && a.toLowerCase() === b.toLowerCase();
}

export function explorerLink(base: string | undefined, kind: "address" | "tx", value: string): string | undefined {
  return base ? `${base.replace(/\/$/, "")}/${kind}/${value}` : undefined;
}

export function timeAgo(iso: string, now: Date = new Date()): string {
  const seconds = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

const MAX_UINT256 = (2n ** 256n - 1n).toString();

/**
 * Makes an alert reason readable in a table cell: addresses and hashes are
 * shortened, the maximum uint256 reads "unlimited" and large amounts get digit
 * groups. The full text stays available as the cell's title.
 */
export function readableReason(reason: string): string {
  return reason
    .replace(/0x[0-9a-fA-F]{64}\b/g, (h) => `${h.slice(0, 10)}…`)
    .replace(/0x[0-9a-fA-F]{40}\b/g, (a) => shortAddress(a))
    .replace(/\b\d{4,}\b/g, (n) => (n === MAX_UINT256 ? "unlimited" : formatBaseUnits(n)));
}
