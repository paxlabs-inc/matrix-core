// contracts/MyNFT.sol
// SPDX-License-Identifier: LicenseRef-Paxlabs-Tachyon-Protocol
pragma solidity ^0.8.24;

import {ERC721} from "../../token/ERC721/ERC721.sol";

contract MyNFT is ERC721 {
    constructor() ERC721("MyNFT", "MNFT") {}
}
