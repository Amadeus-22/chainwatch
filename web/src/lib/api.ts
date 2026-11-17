// Typed client for the chainwatch /v1 API. The session is an HTTP-only cookie
// the browser sends automatically; this code never sees the token.

export type Severity = "medium" | "high" | "critical";
export type Role = "owner" | "member";

export interface Chain {
  id: number;
  name: string;
  explorer_url?: string;
}

export interface Organization {
  id: number;
  name: string;
  role: Role;
  created_at: string;
}

export interface Me {
  address: string;
  organizations: Organization[];
}

export interface Wallet {
  chain_id: number;
  address: string;
  label: string;
  created_at: string;
}

export interface Allowance {
  chain_id: number;
  owner: string;
  token: string;
  spender: string;
  amount: string;
  unlimited: boolean;
  risk: Severity;
  updated_block: number;
}

export interface Alert {
  id: number;
  chain_id: number;
  wallet: string;
  rule: string;
  severity: Severity;
  reason: string;
  token: string;
  tx_hash: string;
  block_number: number;
  status: "pending" | "delivered";
  created_at: string;
}

export interface Member {
  address: string;
  role: Role;
  created_at: string;
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) {
    return undefined as T;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = (data as { error?: { code?: string; message?: string } }).error;
    throw new ApiError(res.status, err?.code ?? "http_error", err?.message ?? `HTTP ${res.status}`);
  }
  return data as T;
}

const org = (id: number) => `/v1/orgs/${id}`;

export const api = {
  chains: () => request<{ chains: Chain[] }>("GET", "/v1/chains").then((r) => r.chains),
  nonce: () => request<{ nonce: string; expires_at: string }>("GET", "/v1/auth/nonce"),
  verify: (message: string, signature: string) =>
    request<{ address: string; expires_at: string }>("POST", "/v1/auth/verify", { message, signature }),
  logout: () => request<void>("POST", "/v1/auth/logout"),
  me: () => request<Me>("GET", "/v1/me"),
  createOrg: (name: string) => request<Organization>("POST", "/v1/orgs", { name }),
  members: (id: number) => request<{ members: Member[] }>("GET", `${org(id)}/members`).then((r) => r.members),
  addMember: (id: number, address: string, role: Role) =>
    request<Member>("POST", `${org(id)}/members`, { address, role }),
  removeMember: (id: number, address: string) => request<void>("DELETE", `${org(id)}/members/${address}`),
  wallets: (id: number) => request<{ wallets: Wallet[] }>("GET", `${org(id)}/wallets`).then((r) => r.wallets),
  addWallet: (id: number, chainId: number, address: string, label: string) =>
    request<Wallet>("POST", `${org(id)}/wallets`, { chain_id: chainId, address, label }),
  removeWallet: (id: number, chainId: number, address: string) =>
    request<void>("DELETE", `${org(id)}/wallets/${chainId}/${address}`),
  allowances: (id: number) =>
    request<{ allowances: Allowance[] }>("GET", `${org(id)}/allowances`).then((r) => r.allowances),
  alerts: (id: number, limit = 50) =>
    request<{ alerts: Alert[] }>("GET", `${org(id)}/alerts?limit=${limit}`).then((r) => r.alerts),
};
