// (c) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// SPDX-License-Identifier: Ecosystem

pragma solidity 0.8.23;

interface IRewardCalculator {
    function calculateReward(
        uint64 stakeAmount,
        uint64 startTime,
        uint64 endTime,
        uint256 initialSupply,
        uint256 endSupply
    ) external returns (uint256);
}
