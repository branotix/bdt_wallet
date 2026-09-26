// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import "@openzeppelin/contracts/access/Ownable.sol";

/// @title BDT Token
/// @notice Fixed-supply BEP-20 token. On-chain transfers only happen between
/// your hot wallet and external user wallets (deposit/withdraw). All
/// internal (user-to-user) transfers are handled off-chain in Postgres.
contract BDTToken is ERC20, Ownable {
    /// @param initialSupply Total supply in whole tokens (multiplied by 10^decimals internally)
    constructor(uint256 initialSupply) ERC20("BDT Token", "BDT") Ownable(msg.sender) {
        _mint(msg.sender, initialSupply * 10 ** decimals());
        // No mint() function exists anywhere else in this contract.
        // Supply is fixed forever after this line.
    }
}
