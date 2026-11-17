import { describe, expect, it } from "vitest";
import { decodeFunctionData, erc20Abi } from "viem";
import { revokeCalldata, revokeTransaction } from "./revoke";

const spender = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC";

describe("revokeCalldata", () => {
  it("encodes approve(spender, 0) byte for byte", () => {
    expect(revokeCalldata(spender)).toBe(
      "0x095ea7b3" + // selector of approve(address,uint256)
        "0000000000000000000000003c44cdddb6a900fa2b585dd299e03d12fa4293bc" +
        "0000000000000000000000000000000000000000000000000000000000000000",
    );
  });

  it("decodes back to approve with a zero amount", () => {
    const { functionName, args } = decodeFunctionData({ abi: erc20Abi, data: revokeCalldata(spender.toLowerCase()) });
    expect(functionName).toBe("approve");
    expect(args).toEqual([spender, 0n]);
  });

  it("rejects a malformed spender", () => {
    expect(() => revokeCalldata("0x1234")).toThrow();
  });
});

describe("revokeTransaction", () => {
  it("is sent by the owner to the token contract with no value", () => {
    const tx = revokeTransaction(
      "0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
      "0x5fbdb2315678afecb367f032d93f642f64180aa3",
      spender,
    );
    expect(tx).toEqual({
      from: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
      to: "0x5FbDB2315678afecb367f032d93F642f64180aa3",
      data: revokeCalldata(spender),
      value: "0x0",
    });
  });
});
