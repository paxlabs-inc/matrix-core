// ERC-20 transaction builders used by the wallet-backed write tools.

import { encodeCall } from './abi.mjs'
export const erc20 = {
  transfer: (token, to, amount) => ({ to: token, data: encodeCall('transfer(address,uint256)', [to, amount]) }),
  approve: (token, spender, amount) => ({ to: token, data: encodeCall('approve(address,uint256)', [spender, amount]) }),
}
