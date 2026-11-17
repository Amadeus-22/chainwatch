import { useCallback, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { isAddress } from "viem";
import { api, ApiError, type Alert, type Allowance, type Chain, type Me, type Member, type Severity, type Wallet } from "../lib/api";
import { classifyWalletError, ensureChain, sendTransaction } from "../lib/ethereum";
import { checksum, explorerLink, formatBaseUnits, readableReason, sameAddress, shortAddress, timeAgo } from "../lib/format";
import { revokeTransaction } from "../lib/revoke";
import type { WalletState } from "../lib/useWallet";
import { Button, Card, ExternalLink, Mono, Notice, RiskBadge, type NoticeTone } from "./ui";

const REFRESH_MS = 15_000;
const ORG_KEY = "chainwatch.org";
const severityRank: Record<Severity, number> = { medium: 1, high: 2, critical: 3 };

interface Message {
  tone: NoticeTone;
  text: ReactNode;
}

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : classifyWalletError(err).message;
}

export function Dashboard({ me, chains, wallet }: { me: Me; chains: Chain[]; wallet: WalletState }) {
  const [orgs, setOrgs] = useState(me.organizations);
  const [orgId, setOrgId] = useState<number | undefined>(() => {
    const saved = Number(localStorage.getItem(ORG_KEY));
    return me.organizations.find((o) => o.id === saved)?.id ?? me.organizations[0]?.id;
  });
  const [wallets, setWallets] = useState<Wallet[]>([]);
  const [allowances, setAllowances] = useState<Allowance[]>([]);
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [members, setMembers] = useState<Member[]>([]);
  const [messages, setMessages] = useState<Message[]>([]);
  const [loaded, setLoaded] = useState(false);

  const org = orgs.find((o) => o.id === orgId);
  const chainById = useMemo(() => new Map(chains.map((c) => [c.id, c])), [chains]);
  const say = (tone: NoticeTone, text: ReactNode) => setMessages((m) => [...m, { tone, text }]);

  const load = useCallback(async () => {
    if (orgId === undefined) return;
    try {
      const [w, a, al, m] = await Promise.all([api.wallets(orgId), api.allowances(orgId), api.alerts(orgId), api.members(orgId)]);
      setWallets(w);
      setAllowances(a);
      setAlerts(al);
      setMembers(m);
      setLoaded(true);
    } catch (err) {
      say("error", `Could not load organization data: ${errorText(err)}`);
    }
  }, [orgId]);

  useEffect(() => {
    setLoaded(false);
    if (orgId !== undefined) localStorage.setItem(ORG_KEY, String(orgId));
    void load();
    const t = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(t);
  }, [orgId, load]);

  const revoke = async (a: Allowance) => {
    const p = wallet.provider;
    const chain = chainById.get(a.chain_id);
    if (!p) {
      say("warning", "No Ethereum wallet detected. Install MetaMask to revoke allowances.");
      return;
    }
    if (!sameAddress(wallet.account, a.owner)) {
      say("warning", `Only the owner can revoke. Select ${checksum(a.owner)} in MetaMask, then click Revoke again.`);
      return;
    }
    try {
      await ensureChain(p, a.chain_id);
      const hash = await sendTransaction(p, revokeTransaction(a.owner, a.token, a.spender));
      say(
        "success",
        <>
          Revoke transaction sent on {chain?.name ?? `chain ${a.chain_id}`}:{" "}
          <ExternalLink href={explorerLink(chain?.explorer_url, "tx", hash)}>
            <Mono>{hash}</Mono>
          </ExternalLink>
          . The allowance disappears here once the transaction is confirmed and indexed.
        </>,
      );
    } catch (err) {
      const e = classifyWalletError(err);
      say(e.kind === "rejected" ? "warning" : "error", e.kind === "rejected" ? "You rejected the transaction; nothing was sent." : e.message);
    }
  };

  if (!org) {
    return <CreateOrg onCreated={(o) => (setOrgs((x) => [...x, o]), setOrgId(o.id))} onError={(e) => say("error", e)} />;
  }

  const byChain = chains
    .map((c) => ({ chain: c, wallets: wallets.filter((w) => w.chain_id === c.id) }))
    .filter((g) => g.wallets.length > 0);

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <label className="text-sm text-slate-600" htmlFor="org">
          Organization
        </label>
        <select
          id="org"
          value={org.id}
          onChange={(e) => setOrgId(Number(e.target.value))}
          className="rounded-lg border border-slate-300 bg-white px-2 py-1.5 text-sm"
        >
          {orgs.map((o) => (
            <option key={o.id} value={o.id}>
              {o.name} ({o.role})
            </option>
          ))}
        </select>
        <CreateOrg compact onCreated={(o) => (setOrgs((x) => [...x, o]), setOrgId(o.id))} onError={(e) => say("error", e)} />
      </div>

      {messages.map((m, i) => (
        <Notice key={i} tone={m.tone} onClose={() => setMessages((all) => all.filter((_, j) => j !== i))}>
          {m.text}
        </Notice>
      ))}

      <Summary wallets={wallets} allowances={allowances} alerts={alerts} />

      <Card title="Watched wallets" actions={<AddWallet orgId={org.id} chains={chains} wallet={wallet} onAdded={load} onError={(e) => say("error", e)} />}>
        {!loaded ? (
          <p className="text-sm text-slate-500">Loading…</p>
        ) : byChain.length === 0 ? (
          <p className="text-sm text-slate-500">No wallets yet. Add a treasury, fund or multisig address to start monitoring its approvals.</p>
        ) : (
          <div className="space-y-6">
            {byChain.map(({ chain, wallets: ws }) => (
              <div key={chain.id}>
                <h3 className="mb-2 text-sm font-semibold text-slate-500 uppercase">{chain.name}</h3>
                <div className="space-y-4">
                  {ws.map((w) => (
                    <WalletRow
                      key={`${w.chain_id}:${w.address}`}
                      wallet={w}
                      chain={chain}
                      allowances={allowances.filter((a) => a.chain_id === w.chain_id && sameAddress(a.owner, w.address))}
                      connected={wallet.account}
                      onRevoke={revoke}
                      onRemove={async () => {
                        try {
                          await api.removeWallet(org.id, w.chain_id, w.address);
                          await load();
                        } catch (err) {
                          say("error", errorText(err));
                        }
                      }}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      <Card title="Alerts">
        {alerts.length === 0 ? (
          <p className="text-sm text-slate-500">No alerts. Risky approvals and drains on watched wallets appear here.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs text-slate-500 uppercase">
                <tr>
                  <th className="py-2 pr-4">Severity</th>
                  <th className="py-2 pr-4">Rule</th>
                  <th className="py-2 pr-4">Wallet</th>
                  <th className="py-2 pr-4">Details</th>
                  <th className="py-2 pr-4">Transaction</th>
                  <th className="py-2">When</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {alerts.map((a) => {
                  const chain = chainById.get(a.chain_id);
                  return (
                    <tr key={a.id} className="align-top">
                      <td className="py-2 pr-4">
                        <RiskBadge level={a.severity} />
                      </td>
                      <td className="py-2 pr-4 font-mono text-xs">{a.rule}</td>
                      <td className="py-2 pr-4">
                        <Mono>{shortAddress(a.wallet)}</Mono>
                        <div className="text-xs text-slate-500">{chain?.name ?? `chain ${a.chain_id}`}</div>
                      </td>
                      <td className="max-w-md py-2 pr-4 text-slate-700" title={a.reason}>
                        {readableReason(a.reason)}
                      </td>
                      <td className="py-2 pr-4">
                        <ExternalLink href={explorerLink(chain?.explorer_url, "tx", a.tx_hash)}>
                          <Mono>{a.tx_hash.slice(0, 10)}…</Mono>
                        </ExternalLink>
                      </td>
                      <td className="py-2 whitespace-nowrap text-slate-500">{timeAgo(a.created_at)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Members orgId={org.id} isOwner={org.role === "owner"} me={me.address} members={members} onChanged={load} onError={(e) => say("error", e)} />
    </div>
  );
}

function Summary({ wallets, allowances, alerts }: { wallets: Wallet[]; allowances: Allowance[]; alerts: Alert[] }) {
  const unlimited = allowances.filter((a) => a.unlimited).length;
  const critical = alerts.filter((a) => a.severity === "critical").length;
  const stats = [
    { label: "Watched wallets", value: wallets.length },
    { label: "Open allowances", value: allowances.length },
    { label: "Unlimited allowances", value: unlimited, warn: unlimited > 0 },
    { label: "Critical alerts", value: critical, warn: critical > 0 },
  ];
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      {stats.map((s) => (
        <div key={s.label} className="rounded-xl border border-slate-200 bg-white px-4 py-3 shadow-sm">
          <div className="text-xs text-slate-500">{s.label}</div>
          <div className={`text-2xl font-semibold ${s.warn ? "text-rose-600" : ""}`}>{s.value}</div>
        </div>
      ))}
    </div>
  );
}

function WalletRow(props: {
  wallet: Wallet;
  chain: Chain;
  allowances: Allowance[];
  connected: string | undefined;
  onRevoke: (a: Allowance) => void;
  onRemove: () => void;
}) {
  const { wallet: w, chain, allowances, connected } = props;
  const worst = allowances.reduce<Severity | undefined>(
    (acc, a) => (!acc || severityRank[a.risk] > severityRank[acc] ? a.risk : acc),
    undefined,
  );
  const isOwner = sameAddress(connected, w.address);
  return (
    <div className="rounded-lg border border-slate-200">
      <div className="flex flex-wrap items-center justify-between gap-2 bg-slate-50 px-4 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{w.label || "Unlabelled"}</span>
          <ExternalLink href={explorerLink(chain.explorer_url, "address", checksum(w.address))}>
            <Mono>{checksum(w.address)}</Mono>
          </ExternalLink>
          {worst ? <RiskBadge level={worst} label={`${allowances.length} open · ${worst} risk`} /> : <RiskBadge level="low" label="no open allowances" />}
          {isOwner && <span className="text-xs text-emerald-700">connected</span>}
        </div>
        <Button variant="secondary" onClick={props.onRemove}>
          Stop watching
        </Button>
      </div>
      {allowances.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="text-xs text-slate-500 uppercase">
              <tr>
                <th className="px-4 py-2">Token</th>
                <th className="px-4 py-2">Spender</th>
                <th className="px-4 py-2">Allowance (base units)</th>
                <th className="px-4 py-2">Risk</th>
                <th className="px-4 py-2">Block</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {allowances.map((a) => (
                <tr key={`${a.token}:${a.spender}`}>
                  <td className="px-4 py-2">
                    <ExternalLink href={explorerLink(chain.explorer_url, "address", checksum(a.token))}>
                      <Mono>{shortAddress(a.token)}</Mono>
                    </ExternalLink>
                  </td>
                  <td className="px-4 py-2">
                    <ExternalLink href={explorerLink(chain.explorer_url, "address", checksum(a.spender))}>
                      <Mono>{shortAddress(a.spender)}</Mono>
                    </ExternalLink>
                  </td>
                  <td className="px-4 py-2 font-mono text-xs">{a.unlimited ? "Unlimited" : formatBaseUnits(a.amount)}</td>
                  <td className="px-4 py-2">
                    <RiskBadge level={a.risk} label={a.unlimited ? "high · unlimited" : a.risk} />
                  </td>
                  <td className="px-4 py-2 text-slate-500">{a.updated_block}</td>
                  <td className="px-4 py-2 text-right">
                    <Button
                      variant="danger"
                      onClick={() => props.onRevoke(a)}
                      title={isOwner ? "Send approve(spender, 0) from your wallet" : `Connect ${checksum(w.address)} in MetaMask to revoke`}
                    >
                      Revoke
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function AddWallet(props: { orgId: number; chains: Chain[]; wallet: WalletState; onAdded: () => void; onError: (e: string) => void }) {
  const [open, setOpen] = useState(false);
  const [chainId, setChainId] = useState(props.chains[0]?.id ?? 1);
  const [address, setAddress] = useState("");
  const [label, setLabel] = useState("");
  const valid = isAddress(address, { strict: false });

  const useConnected = async () => {
    try {
      const { account, chainId: current } = await props.wallet.connect();
      if (account) setAddress(account);
      if (props.chains.some((c) => c.id === current)) setChainId(current);
    } catch (err) {
      props.onError(classifyWalletError(err).message);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.addWallet(props.orgId, chainId, address, label);
      setAddress("");
      setLabel("");
      setOpen(false);
      props.onAdded();
    } catch (err) {
      props.onError(errorText(err));
    }
  };

  if (!open) return <Button onClick={() => setOpen(true)}>Add wallet</Button>;
  return (
    <form onSubmit={submit} className="flex w-full flex-wrap items-end gap-2">
      <select value={chainId} onChange={(e) => setChainId(Number(e.target.value))} className="rounded-lg border border-slate-300 px-2 py-1.5 text-sm" aria-label="Chain">
        {props.chains.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name}
          </option>
        ))}
      </select>
      <input
        value={address}
        onChange={(e) => setAddress(e.target.value.trim())}
        placeholder="0x… address"
        aria-label="Address"
        className="min-w-72 flex-1 rounded-lg border border-slate-300 px-2 py-1.5 font-mono text-sm"
      />
      <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="Label (e.g. Treasury)" aria-label="Label" maxLength={200} className="rounded-lg border border-slate-300 px-2 py-1.5 text-sm" />
      <Button variant="secondary" onClick={useConnected} disabled={!props.wallet.provider} title="Fill in the account and network selected in MetaMask">
        Use connected wallet
      </Button>
      <Button type="submit" disabled={!valid}>
        Watch
      </Button>
      <Button variant="secondary" onClick={() => setOpen(false)}>
        Cancel
      </Button>
    </form>
  );
}

function CreateOrg(props: { compact?: boolean; onCreated: (o: Me["organizations"][number]) => void; onError: (e: string) => void }) {
  const [open, setOpen] = useState(!props.compact);
  const [name, setName] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      props.onCreated(await api.createOrg(name));
      setName("");
      if (props.compact) setOpen(false);
    } catch (err) {
      props.onError(errorText(err));
    }
  };
  if (!open) {
    return (
      <Button variant="secondary" onClick={() => setOpen(true)}>
        New organization
      </Button>
    );
  }
  return (
    <form onSubmit={submit} className="flex flex-wrap items-center gap-2">
      <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Organization name" aria-label="Organization name" maxLength={100} className="rounded-lg border border-slate-300 px-2 py-1.5 text-sm" />
      <Button type="submit" disabled={!name.trim()}>
        Create
      </Button>
      {props.compact && (
        <Button variant="secondary" onClick={() => setOpen(false)}>
          Cancel
        </Button>
      )}
    </form>
  );
}

function Members(props: { orgId: number; isOwner: boolean; me: string; members: Member[]; onChanged: () => void; onError: (e: string) => void }) {
  const [address, setAddress] = useState("");
  const [role, setRole] = useState<"member" | "owner">("member");
  const add = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.addMember(props.orgId, address, role);
      setAddress("");
      props.onChanged();
    } catch (err) {
      props.onError(errorText(err));
    }
  };
  const remove = async (addr: string) => {
    try {
      await api.removeMember(props.orgId, addr);
      props.onChanged();
    } catch (err) {
      props.onError(errorText(err));
    }
  };
  return (
    <Card title="Members">
      <ul className="divide-y divide-slate-100 text-sm">
        {props.members.map((m) => (
          <li key={m.address} className="flex items-center justify-between py-2">
            <span>
              <Mono>{checksum(m.address)}</Mono> <span className="text-slate-500">· {m.role}</span>
              {sameAddress(m.address, props.me) && <span className="text-slate-500"> · you</span>}
            </span>
            {(props.isOwner || sameAddress(m.address, props.me)) && (
              <Button variant="secondary" onClick={() => remove(m.address)}>
                {sameAddress(m.address, props.me) ? "Leave" : "Remove"}
              </Button>
            )}
          </li>
        ))}
      </ul>
      {props.isOwner && (
        <form onSubmit={add} className="mt-4 flex flex-wrap items-center gap-2">
          <input value={address} onChange={(e) => setAddress(e.target.value.trim())} placeholder="Member wallet address 0x…" aria-label="Member address" className="min-w-72 flex-1 rounded-lg border border-slate-300 px-2 py-1.5 font-mono text-sm" />
          <select value={role} onChange={(e) => setRole(e.target.value as "member" | "owner")} aria-label="Role" className="rounded-lg border border-slate-300 px-2 py-1.5 text-sm">
            <option value="member">member</option>
            <option value="owner">owner</option>
          </select>
          <Button type="submit" disabled={!isAddress(address, { strict: false })}>
            Add member
          </Button>
        </form>
      )}
    </Card>
  );
}
