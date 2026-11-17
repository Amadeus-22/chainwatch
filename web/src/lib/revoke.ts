import { encodeFunctionData, erc20Abi, getAddress, type Address, type Hex } from "viem";

/** Calldata for ERC-20 approve(spender, 0), which revokes spender's allowance. */
export function revokeCalldata(spender: string): Hex {
  return encodeFunctionData({ abi: erc20Abi, functionName: "approve", args: [getAddress(spender), 0n] });
}

export interface TxRequest {
  from: Address;
  to: Address;
  data: Hex;
  value: Hex;
}

/**
 * The transaction the owner signs in MetaMask to revoke an allowance. chainwatch
 * never holds keys: this is only a request handed to the wallet.
 */
export function revokeTransaction(owner: string, token: string, spender: string): TxRequest {
  return { from: getAddress(owner), to: getAddress(token), data: revokeCalldata(spender), value: "0x0" };
}
