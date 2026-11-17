import { useCallback, useEffect, useState } from "react";
import { Dashboard } from "./components/Dashboard";
import { SignIn } from "./components/SignIn";
import { Button, Notice } from "./components/ui";
import { api, ApiError, type Chain, type Me } from "./lib/api";
import { sameAddress, shortAddress } from "./lib/format";
import { useWallet } from "./lib/useWallet";

export function App() {
  const wallet = useWallet();
  const [chains, setChains] = useState<Chain[]>([]);
  const [me, setMe] = useState<Me | null | undefined>(undefined); // undefined: loading, null: signed out
  const [error, setError] = useState<string>();

  const loadMe = useCallback(async () => {
    try {
      setMe(await api.me());
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setMe(null);
      } else {
        setError(`chainwatch API unreachable: ${err instanceof Error ? err.message : String(err)}`);
        setMe(null);
      }
    }
  }, []);

  useEffect(() => {
    api.chains().then(setChains, (err) => setError(`Could not load networks: ${err instanceof Error ? err.message : err}`));
    void loadMe();
  }, [loadMe]);

  const signOut = async () => {
    await api.logout().catch(() => undefined);
    setMe(null);
  };

  const mismatch = me && wallet.account && !sameAddress(me.address, wallet.account);

  return (
    <div className="mx-auto max-w-6xl space-y-6 px-4 py-6">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-bold tracking-tight">chainwatch</h1>
          <p className="text-sm text-slate-500">Who can move your organization's tokens, across chains.</p>
        </div>
        {me && (
          <div className="flex items-center gap-3 text-sm">
            <span className="font-mono">{shortAddress(me.address)}</span>
            <Button variant="secondary" onClick={signOut}>
              Sign out
            </Button>
          </div>
        )}
      </header>

      {error && (
        <Notice tone="error" onClose={() => setError(undefined)}>
          {error}
        </Notice>
      )}
      {mismatch && (
        <Notice tone="info">
          MetaMask is on {shortAddress(wallet.account!)}, but you are signed in as {shortAddress(me.address)}. Revoking
          works only from the wallet that granted the allowance.
        </Notice>
      )}

      {me === undefined ? (
        <p className="text-sm text-slate-500">Loading…</p>
      ) : me === null ? (
        <SignIn wallet={wallet} chains={chains} onSignedIn={loadMe} />
      ) : (
        <Dashboard me={me} chains={chains} wallet={wallet} />
      )}
    </div>
  );
}
