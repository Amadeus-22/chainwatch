# contracts

Foundry project used only for local demos and end-to-end tests of chainwatch.
Nothing here is meant for a public network.

| File | Purpose |
|---|---|
| `src/TestToken.sol` | minimal ERC-20 with unrestricted `mint` |
| `test/TestToken.t.sol` | unit and fuzz tests, including the drain scenario |
| `script/Deploy.s.sol` | deploys the token and mints to the victim |
| `script/Drain.s.sol` | unlimited `approve` to an EOA, then `transferFrom` of the whole balance |

```sh
git submodule update --init   # forge-std
forge test                    # or `make contracts-test` from the repo root
```

The end-to-end demo (`make demo` at the repo root) runs both scripts against anvil.
