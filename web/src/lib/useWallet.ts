import { useCallback, useEffect, useState } from "react";
import type { Address } from "viem";
import { connectedAccounts, currentChainId, getProvider, requestAccounts, type Eip1193Provider } from "./ethereum";

export interface WalletState {
  provider: Eip1193Provider | undefined;
  account: Address | undefined;
  chainId: number | undefined;
  /** Asks the wallet for access; resolves to the selected account and network. */
  connect: () => Promise<{ account: Address | undefined; chainId: number }>;
}

/** Tracks the injected wallet's selected account and network. */
export function useWallet(): WalletState {
  const provider = getProvider();
  const [account, setAccount] = useState<Address | undefined>();
  const [chainId, setChainId] = useState<number | undefined>();

  useEffect(() => {
    if (!provider) return;
    let live = true;
    connectedAccounts(provider)
      .then((a) => live && setAccount(a[0]))
      .catch(() => undefined);
    currentChainId(provider)
      .then((c) => live && setChainId(c))
      .catch(() => undefined);
    const onAccounts = (...args: unknown[]) => {
      const list = (args[0] as string[] | undefined) ?? [];
      setAccount(list[0] ? (list[0] as Address) : undefined);
    };
    const onChain = (...args: unknown[]) => setChainId(Number.parseInt(String(args[0]), 16));
    provider.on?.("accountsChanged", onAccounts);
    provider.on?.("chainChanged", onChain);
    return () => {
      live = false;
      provider.removeListener?.("accountsChanged", onAccounts);
      provider.removeListener?.("chainChanged", onChain);
    };
  }, [provider]);

  const connect = useCallback(async () => {
    if (!provider) throw new Error("No Ethereum wallet detected.");
    const accounts = await requestAccounts(provider);
    const chain = await currentChainId(provider);
    setAccount(accounts[0]);
    setChainId(chain);
    return { account: accounts[0], chainId: chain };
  }, [provider]);

  return { provider, account, chainId, connect };
}
