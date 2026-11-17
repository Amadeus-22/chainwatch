import { useState } from "react";
import { api, type Chain } from "../lib/api";
import { classifyWalletError, personalSign, switchChain } from "../lib/ethereum";
import { shortAddress } from "../lib/format";
import { buildSiweMessage } from "../lib/siwe";
import type { WalletState } from "../lib/useWallet";
import { Button, Card, Notice } from "./ui";

/** Connect MetaMask, then Sign-In with Ethereum (EIP-4361). */
export function SignIn({ wallet, chains, onSignedIn }: { wallet: WalletState; chains: Chain[]; onSignedIn: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const supported = chains.find((c) => c.id === wallet.chainId);

  if (!wallet.provider) {
    return (
      <Card title="Sign in">
        <Notice tone="warning">
          No Ethereum wallet detected. Install{" "}
          <a className="underline" href="https://metamask.io/download/" target="_blank" rel="noreferrer">
            MetaMask
          </a>{" "}
          (or another EIP-1193 wallet) and reload this page.
        </Notice>
      </Card>
    );
  }

  const run = async (f: () => Promise<void>) => {
    setBusy(true);
    setError(undefined);
    try {
      await f();
    } catch (err) {
      setError(err instanceof Error && err.name === "ApiError" ? err.message : classifyWalletError(err).message);
    } finally {
      setBusy(false);
    }
  };

  const signIn = () =>
    run(async () => {
      if (!wallet.account || !supported) return;
      const { nonce } = await api.nonce();
      const now = new Date();
      const message = buildSiweMessage({
        domain: window.location.host,
        address: wallet.account,
        statement: "Sign in to chainwatch. This request does not send a transaction or cost gas.",
        uri: window.location.origin,
        chainId: supported.id,
        nonce,
        issuedAt: now,
        expirationTime: new Date(now.getTime() + 5 * 60 * 1000),
      });
      const signature = await personalSign(wallet.provider!, message, wallet.account);
      await api.verify(message, signature);
      onSignedIn();
    });

  return (
    <Card title="Sign in with Ethereum">
      <div className="space-y-4 text-sm">
        <p className="text-slate-600">
          Your wallet address is your account. Signing a message proves you control it; it sends no transaction and
          costs no gas. chainwatch never asks for or holds private keys.
        </p>
        {error && (
          <Notice tone="error" onClose={() => setError(undefined)}>
            {error}
          </Notice>
        )}
        {!wallet.account ? (
          <Button onClick={() => run(async () => void (await wallet.connect()))} disabled={busy}>
            Connect MetaMask
          </Button>
        ) : !supported ? (
          <div className="space-y-3">
            <Notice tone="warning">
              Wrong network: your wallet is on chain {wallet.chainId ?? "unknown"}, which this chainwatch does not
              monitor. Switch to one of:
            </Notice>
            <div className="flex flex-wrap gap-2">
              {chains.map((c) => (
                <Button key={c.id} variant="secondary" disabled={busy} onClick={() => run(() => switchChain(wallet.provider!, c.id))}>
                  {c.name}
                </Button>
              ))}
            </div>
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            <span>
              Connected as <span className="font-mono">{shortAddress(wallet.account)}</span> on {supported.name}
            </span>
            <Button onClick={signIn} disabled={busy}>
              {busy ? "Check your wallet…" : "Sign in"}
            </Button>
          </div>
        )}
      </div>
    </Card>
  );
}
