package teleporter

import (
	"context"
	"math/big"

	"github.com/ava-labs/subnet-evm/accounts/abi/bind"
	teleportermessenger "github.com/ava-labs/teleporter/abi-bindings/go/teleporter/TeleporterMessenger"
	localnetwork "github.com/ava-labs/teleporter/tests/network"
	"github.com/ava-labs/teleporter/tests/utils"
	"github.com/ethereum/go-ethereum/common"
	. "github.com/onsi/gomega"
)

// Tests basic one-way send from L1 A to L1 B and vice versa
func BasicSendReceive(network *localnetwork.LocalNetwork, teleporter utils.TeleporterTestInfo) {
	L1AInfo := network.GetPrimaryNetworkInfo()
	L1BInfo, _ := network.GetTwoL1s()
	teleporterContractAddress := teleporter.TeleporterMessengerAddress(L1AInfo)
	fundedAddress, fundedKey := network.GetFundedAccountInfo()

	// Send a transaction to L1 A to issue a ICM Message from the Teleporter contract to L1 B
	ctx := context.Background()

	// Clear the receipt queue from L1 B -> L1 A to have a clean slate for the test flow.
	// This is only done if the test non-external networks because external networks may have
	// an arbitrarily high number of receipts to be cleared from a given queue from unrelated messages.
	network.ClearReceiptQueue(ctx, teleporter, fundedKey, L1BInfo, L1AInfo)

	feeAmount := big.NewInt(1)
	feeTokenAddress, feeToken := utils.DeployExampleERC20(
		ctx,
		fundedKey,
		L1AInfo,
	)
	utils.ERC20Approve(
		ctx,
		feeToken,
		teleporterContractAddress,
		big.NewInt(0).Mul(big.NewInt(1e18),
			big.NewInt(10)),
		L1AInfo,
		fundedKey,
	)

	sendCrossChainMessageInput := teleportermessenger.TeleporterMessageInput{
		DestinationBlockchainID: L1BInfo.BlockchainID,
		DestinationAddress:      fundedAddress,
		FeeInfo: teleportermessenger.TeleporterFeeInfo{
			FeeTokenAddress: feeTokenAddress,
			Amount:          feeAmount,
		},
		RequiredGasLimit:        big.NewInt(1),
		AllowedRelayerAddresses: []common.Address{},
		Message:                 []byte{1, 2, 3, 4},
	}

	receipt, teleporterMessageID := utils.SendCrossChainMessageAndWaitForAcceptance(
		ctx,
		teleporter.TeleporterMessenger(L1AInfo),
		L1AInfo,
		L1BInfo,
		sendCrossChainMessageInput,
		fundedKey,
	)
	expectedReceiptID := teleporterMessageID

	// Relay the message to the destination
	deliveryReceipt := teleporter.RelayTeleporterMessage(ctx, receipt, L1AInfo, L1BInfo, true, fundedKey)
	receiveEvent, err := utils.GetEventFromLogs(
		deliveryReceipt.Logs,
		teleporter.TeleporterMessenger(L1BInfo).ParseReceiveCrossChainMessage)
	Expect(err).Should(BeNil())

	// Check Teleporter message received on the destination
	delivered, err := teleporter.TeleporterMessenger(L1BInfo).MessageReceived(
		&bind.CallOpts{}, teleporterMessageID,
	)
	Expect(err).Should(BeNil())
	Expect(delivered).Should(BeTrue())

	// Send a transaction to L1 B to issue a ICM Message from the Teleporter contract to L1 A
	sendCrossChainMessageInput.DestinationBlockchainID = L1AInfo.BlockchainID
	sendCrossChainMessageInput.FeeInfo.Amount = big.NewInt(0)
	receipt, teleporterMessageID = utils.SendCrossChainMessageAndWaitForAcceptance(
		ctx,
		teleporter.TeleporterMessenger(L1BInfo),
		L1BInfo,
		L1AInfo,
		sendCrossChainMessageInput,
		fundedKey,
	)

	// Relay the message to the destination
	deliveryReceipt = teleporter.RelayTeleporterMessage(ctx, receipt, L1BInfo, L1AInfo, true, fundedKey)

	Expect(utils.CheckReceiptReceived(
		deliveryReceipt,
		expectedReceiptID,
		teleporter.TeleporterMessenger(L1AInfo))).Should(BeTrue())

	// Check Teleporter message received on the destination
	delivered, err = teleporter.TeleporterMessenger(L1AInfo).MessageReceived(
		&bind.CallOpts{}, teleporterMessageID,
	)
	Expect(err).Should(BeNil())
	Expect(delivered).Should(BeTrue())

	// If the reward address of the message from A->B is the funded address, which is able to send
	// transactions on L1 A, then redeem the rewards.
	if receiveEvent.RewardRedeemer == fundedAddress {
		utils.RedeemRelayerRewardsAndConfirm(
			ctx, teleporter.TeleporterMessenger(L1AInfo), L1AInfo, feeToken, feeTokenAddress, fundedKey, feeAmount,
		)
	}
}
