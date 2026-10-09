// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @title MockStablecoin
/// @notice A six-decimal ERC-20 that behaves like USDC for local and testnet
///         work. Anyone may mint. Deploy to test networks only.
/// @dev Minting is refused on any chain id outside Waybill's test allowlist,
///      mirroring the off-chain test-money guard.
contract MockStablecoin is ERC20 {
    error NotATestNetwork(uint256 chainId);

    constructor(string memory name_, string memory symbol_) ERC20(name_, symbol_) {
        _requireTestNetwork();
    }

    /// @notice USDC and USDT use six decimals; so does this token.
    function decimals() public pure override returns (uint8) {
        return 6;
    }

    /// @notice Mint `amount` minor units (1 token = 1_000_000) to `to`.
    function mint(address to, uint256 amount) external {
        _requireTestNetwork();
        _mint(to, amount);
    }

    function _requireTestNetwork() private view {
        uint256 id = block.chainid;
        if (id != 31337 && id != 84532 && id != 11155111) revert NotATestNetwork(id);
    }
}
