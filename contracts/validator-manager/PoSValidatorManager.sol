// (c) 2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// SPDX-License-Identifier: Ecosystem

pragma solidity 0.8.25;

import {ValidatorManager} from "./ValidatorManager.sol";
import {ValidatorMessages} from "./ValidatorMessages.sol";
import {
    Delegator,
    DelegatorStatus,
    IPoSValidatorManager,
    PoSValidatorInfo,
    PoSValidatorManagerSettings
} from "./interfaces/IPoSValidatorManager.sol";
import {
    Validator,
    ValidatorRegistrationInput,
    ValidatorStatus
} from "./interfaces/IValidatorManager.sol";
import {IRewardCalculator} from "./interfaces/IRewardCalculator.sol";
import {WarpMessage} from
    "@avalabs/subnet-evm-contracts@1.2.0/contracts/interfaces/IWarpMessenger.sol";
import {ReentrancyGuardUpgradeable} from
    "@openzeppelin/contracts-upgradeable@5.0.2/utils/ReentrancyGuardUpgradeable.sol";

/**
 * @dev Implementation of the {IPoSValidatorManager} interface.
 *
 * @custom:security-contact https://github.com/ava-labs/icm-contracts/blob/main/SECURITY.md
 */
abstract contract PoSValidatorManager is
    IPoSValidatorManager,
    ValidatorManager,
    ReentrancyGuardUpgradeable
{
    // solhint-disable private-vars-leading-underscore
    /// @custom:storage-location erc7201:avalanche-icm.storage.PoSValidatorManager
    struct PoSValidatorManagerStorage {
        /// @notice The minimum amount of stake required to be a validator.
        uint256 _minimumStakeAmount;
        /// @notice The maximum amount of stake allowed to be a validator.
        uint256 _maximumStakeAmount;
        /// @notice The minimum amount of time in seconds a validator must be staked for. Must be at least {_churnPeriodSeconds}.
        uint64 _minimumStakeDuration;
        /// @notice The minimum delegation fee percentage, in basis points, required to delegate to a validator.
        uint16 _minimumDelegationFeeBips;
        /**
         * @notice A multiplier applied to validator's initial stake amount to determine
         * the maximum amount of stake a validator can have with delegations.
         * Note: Setting this value to 1 would disable delegations to validators, since
         * the maximum stake would be equal to the initial stake.
         */
        uint64 _maximumStakeMultiplier;
        /// @notice The factor used to convert between weight and value.
        uint256 _weightToValueFactor;
        /// @notice The reward calculator for this validator manager.
        IRewardCalculator _rewardCalculator;
        /// @notice The ID of the blockchain that submits uptime proofs. This must be a blockchain validated by the l1ID that this contract manages.
        bytes32 _uptimeBlockchainID;
        /// @notice Maps the validation ID to its requirements.
        mapping(bytes32 validationID => PoSValidatorInfo) _posValidatorInfo;
        /// @notice Maps the delegation ID to the delegator information.
        mapping(bytes32 delegationID => Delegator) _delegatorStakes;
        /// @notice Maps the delegation ID to its pending staking rewards.
        mapping(bytes32 delegationID => uint256) _redeemableDelegatorRewards;
        mapping(bytes32 delegationID => address) _delegatorRewardRecipients;
        /// @notice Maps the validation ID to its pending staking rewards.
        mapping(bytes32 validationID => uint256) _redeemableValidatorRewards;
        mapping(bytes32 validationID => address) _rewardRecipients;
    }
    // solhint-enable private-vars-leading-underscore

    // keccak256(abi.encode(uint256(keccak256("avalanche-icm.storage.PoSValidatorManager")) - 1)) & ~bytes32(uint256(0xff));
    bytes32 public constant POS_VALIDATOR_MANAGER_STORAGE_LOCATION =
        0x4317713f7ecbdddd4bc99e95d903adedaa883b2e7c2551610bd13e2c7e473d00;

    uint8 public constant MAXIMUM_STAKE_MULTIPLIER_LIMIT = 10;

    uint16 public constant MAXIMUM_DELEGATION_FEE_BIPS = 10000;

    uint16 public constant BIPS_CONVERSION_FACTOR = 10000;

    error InvalidDelegationFee(uint16 delegationFeeBips);
    error InvalidDelegationID(bytes32 delegationID);
    error InvalidDelegatorStatus(DelegatorStatus status);
    error InvalidNonce(uint64 nonce);
    error InvalidRewardRecipient(address rewardRecipient);
    error InvalidStakeAmount(uint256 stakeAmount);
    error InvalidMinStakeDuration(uint64 minStakeDuration);
    error InvalidStakeMultiplier(uint8 maximumStakeMultiplier);
    error MaxWeightExceeded(uint64 newValidatorWeight);
    error MinStakeDurationNotPassed(uint64 endTime);
    error UnauthorizedOwner(address sender);
    error ValidatorNotPoS(bytes32 validationID);
    error ValidatorIneligibleForRewards(bytes32 validationID);
    error DelegatorIneligibleForRewards(bytes32 delegationID);
    error ZeroWeightToValueFactor();
    error InvalidUptimeBlockchainID(bytes32 uptimeBlockchainID);

    // solhint-disable ordering
    /**
     * @dev This storage is visible to child contracts for convenience.
     *      External getters would be better practice, but code size limitations are preventing this.
     *      Child contracts should probably never write to this storage.
     */
    function _getPoSValidatorManagerStorage()
        internal
        pure
        returns (PoSValidatorManagerStorage storage $)
    {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := POS_VALIDATOR_MANAGER_STORAGE_LOCATION
        }
    }

    // solhint-disable-next-line func-name-mixedcase
    function __POS_Validator_Manager_init(
        PoSValidatorManagerSettings calldata settings
    ) internal onlyInitializing {
        __ValidatorManager_init(settings.baseSettings);
        __ReentrancyGuard_init();
        __POS_Validator_Manager_init_unchained({
            minimumStakeAmount: settings.minimumStakeAmount,
            maximumStakeAmount: settings.maximumStakeAmount,
            minimumStakeDuration: settings.minimumStakeDuration,
            minimumDelegationFeeBips: settings.minimumDelegationFeeBips,
            maximumStakeMultiplier: settings.maximumStakeMultiplier,
            weightToValueFactor: settings.weightToValueFactor,
            rewardCalculator: settings.rewardCalculator,
            uptimeBlockchainID: settings.uptimeBlockchainID
        });
    }

    // solhint-disable-next-line func-name-mixedcase
    function __POS_Validator_Manager_init_unchained(
        uint256 minimumStakeAmount,
        uint256 maximumStakeAmount,
        uint64 minimumStakeDuration,
        uint16 minimumDelegationFeeBips,
        uint8 maximumStakeMultiplier,
        uint256 weightToValueFactor,
        IRewardCalculator rewardCalculator,
        bytes32 uptimeBlockchainID
    ) internal onlyInitializing {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();
        if (minimumDelegationFeeBips == 0 || minimumDelegationFeeBips > MAXIMUM_DELEGATION_FEE_BIPS)
        {
            revert InvalidDelegationFee(minimumDelegationFeeBips);
        }
        if (minimumStakeAmount > maximumStakeAmount) {
            revert InvalidStakeAmount(minimumStakeAmount);
        }
        if (maximumStakeMultiplier == 0 || maximumStakeMultiplier > MAXIMUM_STAKE_MULTIPLIER_LIMIT)
        {
            revert InvalidStakeMultiplier(maximumStakeMultiplier);
        }
        // Minimum stake duration should be at least one churn period in order to prevent churn tracker abuse.
        if (minimumStakeDuration < _getChurnPeriodSeconds()) {
            revert InvalidMinStakeDuration(minimumStakeDuration);
        }
        if (weightToValueFactor == 0) {
            revert ZeroWeightToValueFactor();
        }
        if (uptimeBlockchainID == bytes32(0)) {
            revert InvalidUptimeBlockchainID(uptimeBlockchainID);
        }

        $._minimumStakeAmount = minimumStakeAmount;
        $._maximumStakeAmount = maximumStakeAmount;
        $._minimumStakeDuration = minimumStakeDuration;
        $._minimumDelegationFeeBips = minimumDelegationFeeBips;
        $._maximumStakeMultiplier = maximumStakeMultiplier;
        $._weightToValueFactor = weightToValueFactor;
        $._rewardCalculator = rewardCalculator;
        $._uptimeBlockchainID = uptimeBlockchainID;
    }

    /**
     * @notice See {IPoSValidatorManager-submitUptimeProof}.
     */
    function submitUptimeProof(bytes32 validationID, uint32 messageIndex) external {
        if (!_isPoSValidator(validationID)) {
            revert ValidatorNotPoS(validationID);
        }
        ValidatorStatus status = getValidator(validationID).status;
        if (status != ValidatorStatus.Active) {
            revert InvalidValidatorStatus(status);
        }

        // Uptime proofs include the absolute number of seconds the validator has been active.
        _updateUptime(validationID, messageIndex);
    }

    /**
     * @notice See {IPoSValidatorManager-claimDelegationFees}.
     */
    function claimDelegationFees(
        bytes32 validationID
    ) external {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();

        ValidatorStatus status = getValidator(validationID).status;
        if (status != ValidatorStatus.Completed) {
            revert InvalidValidatorStatus(status);
        }

        if ($._posValidatorInfo[validationID].owner != _msgSender()) {
            revert UnauthorizedOwner(_msgSender());
        }

        _withdrawValidationRewards($._posValidatorInfo[validationID].owner, validationID);
    }

    /**
     * @notice See {IPoSValidatorManager-initializeEndValidation}.
     */
    function initializeEndValidation(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex
    ) external {
        _initializeEndValidationWithCheck(
            validationID, includeUptimeProof, messageIndex, address(0)
        );
    }

    /**
     * @notice See {IPoSValidatorManager-initializeEndValidation}.
     */
    function initializeEndValidation(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex,
        address rewardRecipient
    ) external {
        _initializeEndValidationWithCheck(
            validationID, includeUptimeProof, messageIndex, rewardRecipient
        );
    }

    function _initializeEndValidationWithCheck(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex,
        address rewardRecipient
    ) internal {
        if (
            !_initializeEndPoSValidation(
                validationID, includeUptimeProof, messageIndex, rewardRecipient
            )
        ) {
            revert ValidatorIneligibleForRewards(validationID);
        }
    }

    /**
     * @notice See {IPoSValidatorManager-forceInitializeEndValidation}.
     */
    function forceInitializeEndValidation(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex
    ) external {
        // Ignore the return value here to force end validation, regardless of possible missed rewards
        _initializeEndPoSValidation(validationID, includeUptimeProof, messageIndex, address(0));
    }

    /**
     * @notice See {IPoSValidatorManager-forceInitializeEndValidation}.
     */
    function forceInitializeEndValidation(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex,
        address rewardRecipient
    ) external {
        // Ignore the return value here to force end validation, regardless of possible missed rewards
        _initializeEndPoSValidation(validationID, includeUptimeProof, messageIndex, rewardRecipient);
    }

    function changeValidatorRewardRecipient(
        bytes32 validationID,
        address rewardRecipient
    ) external {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();

        if (rewardRecipient == address(0)) {
            revert InvalidRewardRecipient(rewardRecipient);
        }

        if ($._posValidatorInfo[validationID].owner != _msgSender()) {
            revert UnauthorizedOwner(_msgSender());
        }

        $._rewardRecipients[validationID] = rewardRecipient;
    }

    //solhint-disable no-empty-blocks
    function changeDelegatorRewardRecipient(
        bytes32 delegationID,
        address rewardRecipient
    ) external {}

    /**
     * @dev Helper function that initializes the end of a PoS validation period.
     * Returns false if it is possible for the validator to claim rewards, but it is not eligible.
     * Returns true otherwise.
     */
    function _initializeEndPoSValidation(
        bytes32 validationID,
        bool includeUptimeProof,
        uint32 messageIndex,
        address rewardRecipient
    ) internal virtual returns (bool) {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();

        Validator memory validator = _initializeEndValidation(validationID);

        // Non-PoS validators are required to boostrap the network, but are not eligible for rewards.
        if (!_isPoSValidator(validationID)) {
            return true;
        }

        // PoS validations can only be ended by their owners.
        if ($._posValidatorInfo[validationID].owner != _msgSender()) {
            revert UnauthorizedOwner(_msgSender());
        }

        // Check that minimum stake duration has passed.
        if (
            validator.endedAt
                < validator.startedAt + $._posValidatorInfo[validationID].minStakeDuration
        ) {
            revert MinStakeDurationNotPassed(validator.endedAt);
        }

        // Uptime proofs include the absolute number of seconds the validator has been active.
        uint64 uptimeSeconds;
        if (includeUptimeProof) {
            uptimeSeconds = _updateUptime(validationID, messageIndex);
        } else {
            uptimeSeconds = $._posValidatorInfo[validationID].uptimeSeconds;
        }

        uint256 reward = $._rewardCalculator.calculateReward({
            stakeAmount: weightToValue(validator.startingWeight),
            validatorStartTime: validator.startedAt,
            stakingStartTime: validator.startedAt,
            stakingEndTime: validator.endedAt,
            uptimeSeconds: uptimeSeconds
        });

        if (rewardRecipient == address(0)) {
            rewardRecipient = $._posValidatorInfo[validationID].owner;
        }

        $._redeemableValidatorRewards[validationID] += reward;
        $._rewardRecipients[validationID] = rewardRecipient;

        return (reward > 0);
    }

    /**
     * @notice See {IValidatorManager-completeEndValidation}.
     */
    function completeEndValidation(
        uint32 messageIndex
    ) external nonReentrant {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();

        (bytes32 validationID, Validator memory validator) = _completeEndValidation(messageIndex);

        // Return now if this was originally a PoA validator that was later migrated to this PoS manager,
        // or the validator was part of the initial validator set.
        if (!_isPoSValidator(validationID)) {
            return;
        }

        address owner = $._posValidatorInfo[validationID].owner;
        address rewardRecipient = $._rewardRecipients[validationID];
        delete $._rewardRecipients[validationID];

        // the reward-recipient should always be set, but just in case it isn't, we won't burn the reward
        if (rewardRecipient == address(0)) {
            rewardRecipient = owner;
        }

        // The validator can either be Completed or Invalidated here. We only grant rewards for Completed.
        if (validator.status == ValidatorStatus.Completed) {
            _withdrawValidationRewards(rewardRecipient, validationID);
        }

        // The stake is unlocked whether the validation period is completed or invalidated.
        _unlock(owner, weightToValue(validator.startingWeight));
    }

    /**
     * @dev Helper function that extracts the uptime from a ValidationUptimeMessage Warp message
     * If the uptime is greater than the stored uptime, update the stored uptime.
     */
    function _updateUptime(bytes32 validationID, uint32 messageIndex) internal returns (uint64) {
        (WarpMessage memory warpMessage, bool valid) =
            WARP_MESSENGER.getVerifiedWarpMessage(messageIndex);
        if (!valid) {
            revert InvalidWarpMessage();
        }

        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();
        // The uptime proof must be from the specifed uptime blockchain
        if (warpMessage.sourceChainID != $._uptimeBlockchainID) {
            revert InvalidWarpSourceChainID(warpMessage.sourceChainID);
        }

        // The sender is required to be the zero address so that we know the validator node
        // signed the proof directly, rather than as an arbitrary on-chain message
        if (warpMessage.originSenderAddress != address(0)) {
            revert InvalidWarpOriginSenderAddress(warpMessage.originSenderAddress);
        }
        if (warpMessage.originSenderAddress != address(0)) {
            revert InvalidWarpOriginSenderAddress(warpMessage.originSenderAddress);
        }

        (bytes32 uptimeValidationID, uint64 uptime) =
            ValidatorMessages.unpackValidationUptimeMessage(warpMessage.payload);
        if (validationID != uptimeValidationID) {
            revert InvalidValidationID(validationID);
        }

        if (uptime > $._posValidatorInfo[validationID].uptimeSeconds) {
            $._posValidatorInfo[validationID].uptimeSeconds = uptime;
            emit UptimeUpdated(validationID, uptime);
        } else {
            uptime = $._posValidatorInfo[validationID].uptimeSeconds;
        }

        return uptime;
    }

    function _initializeValidatorRegistration(
        ValidatorRegistrationInput calldata registrationInput,
        uint16 delegationFeeBips,
        uint64 minStakeDuration,
        uint256 stakeAmount
    ) internal virtual returns (bytes32) {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();
        // Validate and save the validator requirements
        if (
            delegationFeeBips < $._minimumDelegationFeeBips
                || delegationFeeBips > MAXIMUM_DELEGATION_FEE_BIPS
        ) {
            revert InvalidDelegationFee(delegationFeeBips);
        }

        if (minStakeDuration < $._minimumStakeDuration) {
            revert InvalidMinStakeDuration(minStakeDuration);
        }

        // Ensure the weight is within the valid range.
        if (stakeAmount < $._minimumStakeAmount || stakeAmount > $._maximumStakeAmount) {
            revert InvalidStakeAmount(stakeAmount);
        }

        // Lock the stake in the contract.
        uint256 lockedValue = _lock(stakeAmount);

        uint64 weight = valueToWeight(lockedValue);
        bytes32 validationID = _initializeValidatorRegistration(registrationInput, weight);

        address owner = _msgSender();

        $._posValidatorInfo[validationID].owner = owner;
        $._posValidatorInfo[validationID].delegationFeeBips = delegationFeeBips;
        $._posValidatorInfo[validationID].minStakeDuration = minStakeDuration;
        $._posValidatorInfo[validationID].uptimeSeconds = 0;
        $._rewardRecipients[validationID] = owner;

        return validationID;
    }

    /**
     * @notice Converts a token value to a weight.
     * @param value Token value to convert.
     */
    function valueToWeight(
        uint256 value
    ) public view returns (uint64) {
        uint256 weight = value / _getPoSValidatorManagerStorage()._weightToValueFactor;
        if (weight == 0 || weight > type(uint64).max) {
            revert InvalidStakeAmount(value);
        }
        return uint64(weight);
    }

    /**
     * @notice Converts a weight to a token value.
     * @param weight weight to convert.
     */
    function weightToValue(
        uint64 weight
    ) public view returns (uint256) {
        return uint256(weight) * _getPoSValidatorManagerStorage()._weightToValueFactor;
    }

    /**
     * @notice Locks tokens in this contract.
     * @param value Number of tokens to lock.
     */
    function _lock(
        uint256 value
    ) internal virtual returns (uint256);

    /**
     * @notice Unlocks token to a specific address.
     * @param to Address to send token to.
     * @param value Number of tokens to lock.
     */
    function _unlock(address to, uint256 value) internal virtual;

    //solhint-disable no-empty-blocks
    function _initializeDelegatorRegistration(
        bytes32 validationID,
        address delegatorAddress,
        uint256 delegationAmount
    ) internal returns (bytes32) {}

    /**
     * @notice See {IPoSValidatorManager-completeDelegatorRegistration}.
     */
    //solhint-disable no-empty-blocks
    function completeDelegatorRegistration(
        bytes32, /* delegationID */
        uint32 /* messageIndex */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-initializeEndDelegation}.
     */
    //solhint-disable no-empty-blocks
    function initializeEndDelegation(
        bytes32, /* delegationID */
        bool, /* includeUptimeProof */
        uint32 /* messageIndex */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-initializeEndDelegation}.
     */
    //solhint-disable no-empty-blocks
    function initializeEndDelegation(
        bytes32, /* delegationID */
        bool, /* includeUptimeProof */
        uint32, /* messageIndex */
        address /* rewardRecipient */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-forceInitializeEndDelegation}.
     */

    //solhint-disable no-empty-blocks
    function forceInitializeEndDelegation(
        bytes32, /* delegationID */
        bool, /* includeUptimeProof */
        uint32 /* messageIndex */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-forceInitializeEndDelegation}.
     */
    //solhint-disable no-empty-blocks
    function forceInitializeEndDelegation(
        bytes32, /* delegationID */
        bool, /* includeUptimeProof */
        uint32, /* messageIndex */
        address /* rewardRecipient */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-resendUpdateDelegation}.
     * @dev Resending the latest validator weight with the latest nonce is safe because all weight changes are
     * cumulative, so the latest weight change will always include the weight change for any added delegators.
     */
    //solhint-disable no-empty-blocks
    function resendUpdateDelegation(
        bytes32 /* delegationID */
    ) external {}

    /**
     * @notice See {IPoSValidatorManager-completeEndDelegation}.
     */
    //solhint-disable no-empty-blocks
    function completeEndDelegation(
        bytes32, /* delegationID */
        uint32 /* messageIndex */
    ) external nonReentrant {}

    /**
     * @dev This function must be implemented to mint rewards to validators and delegators.
     */
    function _reward(address account, uint256 amount) internal virtual;

    /**
     * @dev Return true if this is a PoS validator with locked stake. Returns false if this was originally a PoA
     * validator that was later migrated to this PoS manager, or the validator was part of the initial validator set.
     */
    function _isPoSValidator(
        bytes32 validationID
    ) internal view returns (bool) {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();
        return $._posValidatorInfo[validationID].owner != address(0);
    }

    function _withdrawValidationRewards(address rewardRecipient, bytes32 validationID) internal {
        PoSValidatorManagerStorage storage $ = _getPoSValidatorManagerStorage();

        uint256 rewards = $._redeemableValidatorRewards[validationID];
        delete $._redeemableValidatorRewards[validationID];

        _reward(rewardRecipient, rewards);
    }
}
