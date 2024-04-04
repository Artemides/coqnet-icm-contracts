package flows

import (
	"context"
	"math"
	"math/big"

	nativetokendestination "github.com/ava-labs/teleporter-token-bridge/abi-bindings/go/NativeTokenDestination"
	nativetokensource "github.com/ava-labs/teleporter-token-bridge/abi-bindings/go/NativeTokenSource"
	"github.com/ava-labs/teleporter-token-bridge/tests/utils"
	"github.com/ava-labs/teleporter/tests/interfaces"
	teleporterUtils "github.com/ava-labs/teleporter/tests/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	. "github.com/onsi/gomega"
)

var (
	decimalsShift           = big.NewInt(1)
	tokenMultipler          = big.NewInt(int64(math.Pow10(int(decimalsShift.Int64()))))
	initialReserveImbalance = big.NewInt(0).Mul(big.NewInt(1e15), big.NewInt(1e9))
	valueToReceive          = big.NewInt(0).Div(initialReserveImbalance, big.NewInt(4))
	valueToSend             = big.NewInt(0).Div(valueToReceive, tokenMultipler)
	valueToReturn           = big.NewInt(0).Div(valueToReceive, big.NewInt(4))
	multiplyOnReceive       = true
)

/**
 * Deploy a native token source on the primary network
 * Deploys a native token destination to Subnet A
 * Bridges C-Chain native tokens to Subnet A
 * Bridge back tokens from Subnet A to C-Chain
 */
func NativeSourceNativeDestination(network interfaces.Network) {
	cChainInfo := network.GetPrimaryNetworkInfo()
	subnetAInfo, _ := teleporterUtils.GetTwoSubnets(network)
	fundedAddress, fundedKey := network.GetFundedAccountInfo()

	ctx := context.Background()

	// Deploy an example WAVAX on the primary network
	wavaxAddressA, wavaxA := utils.DeployExampleWAVAX(
		ctx,
		fundedKey,
		cChainInfo,
	)

	// Deploy an example WAVAX on the primary network
	wavaxAddressB, _ := utils.DeployExampleWAVAX(
		ctx,
		fundedKey,
		subnetAInfo,
	)

	// Create a NativeTokenSource on the primary network
	nativeTokenSourceAddress, nativeTokenSource := utils.DeployNativeTokenSource(
		ctx,
		fundedKey,
		cChainInfo,
		fundedAddress,
		wavaxAddressA,
	)

	// Deploy an NativeTokenDestination to Subnet A
	nativeTokenDestinationAddress, nativeTokenDestination := utils.DeployNativeTokenDestination(
		ctx,
		subnetAInfo,
		fundedAddress,
		cChainInfo.BlockchainID,
		nativeTokenSourceAddress,
		wavaxAddressB,
		initialReserveImbalance,
		decimalsShift,
		multiplyOnReceive,
	)

	// Generate new recipient to receive bridged tokens
	recipientKey, err := crypto.GenerateKey()
	Expect(err).Should(BeNil())
	recipientAddress := crypto.PubkeyToAddress(recipientKey.PublicKey)

	// Send tokens from C-Chain to recipient on subnet A that don't fully collateralize bridge
	{
		input := nativetokensource.SendTokensInput{
			DestinationBlockchainID:  subnetAInfo.BlockchainID,
			DestinationBridgeAddress: nativeTokenDestinationAddress,
			Recipient:                recipientAddress,
			PrimaryFee:               big.NewInt(0),
			SecondaryFee:             big.NewInt(0),
			AllowedRelayerAddresses:  []common.Address{},
		}

		receipt, bridgedAmount := utils.SendNativeTokenSource(
			ctx,
			cChainInfo,
			nativeTokenSource,
			input,
			valueToSend,
			fundedKey,
		)
		scaledBridgedAmount := teleporterUtils.BigIntMul(bridgedAmount, tokenMultipler)

		receipt = network.RelayMessage(
			ctx,
			receipt,
			cChainInfo,
			subnetAInfo,
			true,
		)

		utils.CheckNativeTokenDestinationMint(
			ctx,
			subnetAInfo,
			nativeTokenDestination,
			recipientAddress,
			receipt,
			big.NewInt(0),
			big.NewInt(0),
		)
		utils.CheckNativeTokenDestinationCollateralize(
			ctx,
			nativeTokenDestination,
			receipt,
			scaledBridgedAmount,
			teleporterUtils.BigIntSub(initialReserveImbalance, scaledBridgedAmount),
		)
	}

	// Send tokens from C-Chain to recipient on subnet A that over-collateralize bridge
	{
		input := nativetokensource.SendTokensInput{
			DestinationBlockchainID:  subnetAInfo.BlockchainID,
			DestinationBridgeAddress: nativeTokenDestinationAddress,
			Recipient:                recipientAddress,
			PrimaryFee:               big.NewInt(0),
			SecondaryFee:             big.NewInt(0),
			AllowedRelayerAddresses:  []common.Address{},
		}

		// Send initialReserveImbalance tokens to over-collateralize bridge
		receipt, _ := utils.SendNativeTokenSource(
			ctx,
			cChainInfo,
			nativeTokenSource,
			input,
			big.NewInt(0).Div(initialReserveImbalance, tokenMultipler),
			fundedKey,
		)

		receipt = network.RelayMessage(
			ctx,
			receipt,
			cChainInfo,
			subnetAInfo,
			true,
		)

		utils.CheckNativeTokenDestinationMint(
			ctx,
			subnetAInfo,
			nativeTokenDestination,
			recipientAddress,
			receipt,
			valueToReceive,
			valueToReceive,
		)
		utils.CheckNativeTokenDestinationCollateralize(
			ctx,
			nativeTokenDestination,
			receipt,
			teleporterUtils.BigIntSub(initialReserveImbalance, valueToReceive),
			big.NewInt(0),
		)
	}

	// Send tokens on Subnet A back for native tokens on C-Chain
	{
		input_A := nativetokendestination.SendTokensInput{
			DestinationBlockchainID:  cChainInfo.BlockchainID,
			DestinationBridgeAddress: nativeTokenSourceAddress,
			Recipient:                recipientAddress,
			PrimaryFee:               big.NewInt(0),
			SecondaryFee:             big.NewInt(0),
			AllowedRelayerAddresses:  []common.Address{},
		}

		receipt, bridgedAmount := utils.SendNativeTokenDestination(
			ctx,
			subnetAInfo,
			nativeTokenDestination,
			input_A,
			valueToReturn,
			recipientKey,
			tokenMultipler,
			multiplyOnReceive,
		)

		receipt = network.RelayMessage(
			ctx,
			receipt,
			subnetAInfo,
			cChainInfo,
			true,
		)

		// Check that the recipient received the tokens
		utils.CheckNativeTokenSourceWithdrawal(
			ctx,
			nativeTokenSourceAddress,
			wavaxA,
			receipt,
			bridgedAmount,
		)

		teleporterUtils.CheckBalance(ctx, recipientAddress, bridgedAmount, cChainInfo.RPCClient)
	}
}
