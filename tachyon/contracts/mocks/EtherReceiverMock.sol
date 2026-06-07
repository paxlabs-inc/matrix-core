// SPDX-License-Identifier: LicenseRef-Paxlabs-Tachyon-Protocol

pragma solidity ^0.8.20;

contract EtherReceiverMock {
    bool private _acceptEther;

    function setAcceptEther(bool acceptEther) public {
        _acceptEther = acceptEther;
    }

    receive() external payable {
        if (!_acceptEther) {
            revert();
        }
    }
}
