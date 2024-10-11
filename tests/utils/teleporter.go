package utils

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"math/big"

	"github.com/ava-labs/avalanchego/ids"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/subnet-evm/accounts/abi/bind"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/precompile/contracts/warp"
	predicateutils "github.com/ava-labs/subnet-evm/predicate"
	"github.com/ava-labs/subnet-evm/rpc"
	validatorsetsig "github.com/ava-labs/teleporter/abi-bindings/go/governance/ValidatorSetSig"
	exampleerc20 "github.com/ava-labs/teleporter/abi-bindings/go/mocks/ExampleERC20"
	teleportermessenger "github.com/ava-labs/teleporter/abi-bindings/go/teleporter/TeleporterMessenger"
	teleporterregistry "github.com/ava-labs/teleporter/abi-bindings/go/teleporter/registry/TeleporterRegistry"
	testmessenger "github.com/ava-labs/teleporter/abi-bindings/go/teleporter/tests/TestMessenger"
	"github.com/ava-labs/teleporter/tests/interfaces"
	deploymentUtils "github.com/ava-labs/teleporter/utils/deployment-utils"
	gasUtils "github.com/ava-labs/teleporter/utils/gas-utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	. "github.com/onsi/gomega"
)

var (
	DefaultTeleporterTransactionGas   uint64 = 300_000
	DefaultTeleporterTransactionValue        = common.Big0
)

type ChainTeleporterInfo struct {
	TeleporterRegistry        *teleporterregistry.TeleporterRegistry
	TeleporterRegistryAddress common.Address

	TeleporterMessenger        *teleportermessenger.TeleporterMessenger
	TeleporterMessengerAddress common.Address
}

type TeleporterTestInfo map[ids.ID]*ChainTeleporterInfo

func NewTeleporterTestInfo(subnets []interfaces.SubnetTestInfo) TeleporterTestInfo {
	t := make(TeleporterTestInfo)
	for _, subnet := range subnets {
		t[subnet.BlockchainID] = &ChainTeleporterInfo{}
	}
	return t
}

func (t TeleporterTestInfo) SetTeleporter(address common.Address, subnet interfaces.SubnetTestInfo) {
	teleporterMessenger, err := teleportermessenger.NewTeleporterMessenger(
		address, subnet.RPCClient,
	)
	Expect(err).Should(BeNil())
	info := t[subnet.BlockchainID]
	info.TeleporterMessengerAddress = address
	info.TeleporterMessenger = teleporterMessenger
}

func (t TeleporterTestInfo) InitializeBlockchainID(subnet interfaces.SubnetTestInfo, fundedKey *ecdsa.PrivateKey) {
	opts, err := bind.NewKeyedTransactorWithChainID(fundedKey, subnet.EVMChainID)
	Expect(err).Should(BeNil())
	tx, err := t[subnet.BlockchainID].TeleporterMessenger.InitializeBlockchainID(opts)
	Expect(err).Should(BeNil())
	WaitForTransactionSuccess(context.Background(), subnet, tx.Hash())
}

func (t TeleporterTestInfo) DeployTeleporterRegistry(subnet interfaces.SubnetTestInfo, deployerKey *ecdsa.PrivateKey) {
	ctx := context.Background()
	entries := []teleporterregistry.ProtocolRegistryEntry{
		{
			Version:         big.NewInt(1),
			ProtocolAddress: t[subnet.BlockchainID].TeleporterMessengerAddress,
		},
	}
	opts, err := bind.NewKeyedTransactorWithChainID(deployerKey, subnet.EVMChainID)
	Expect(err).Should(BeNil())
	teleporterRegistryAddress, tx, teleporterRegistry, err := teleporterregistry.DeployTeleporterRegistry(
		opts, subnet.RPCClient, entries,
	)
	Expect(err).Should(BeNil())
	// Wait for the transaction to be mined
	WaitForTransactionSuccess(ctx, subnet, tx.Hash())

	info := t[subnet.BlockchainID]
	info.TeleporterRegistryAddress = teleporterRegistryAddress
	info.TeleporterRegistry = teleporterRegistry
}

func (t TeleporterTestInfo) DeployTeleporterMessenger(
	ctx context.Context,
	subnet interfaces.SubnetTestInfo,
	transactionBytes []byte,
	deployerAddress common.Address,
	contractAddress common.Address,
	fundedKey *ecdsa.PrivateKey,
) {
	// Fund the deployer address
	fundAmount := big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(11)) // 11 AVAX
	fundDeployerTx := CreateNativeTransferTransaction(
		ctx, subnet, fundedKey, deployerAddress, fundAmount,
	)
	SendTransactionAndWaitForSuccess(ctx, subnet, fundDeployerTx)

	log.Info("Finished funding Teleporter deployer", "blockchainID", subnet.BlockchainID.Hex())

	// Deploy Teleporter contract
	rpcClient, err := rpc.DialContext(
		ctx,
		HttpToRPCURI(subnet.NodeURIs[0], subnet.BlockchainID.String()),
	)
	Expect(err).Should(BeNil())
	defer rpcClient.Close()

	txHash := common.Hash{}
	err = rpcClient.CallContext(ctx, &txHash, "eth_sendRawTransaction", hexutil.Encode(transactionBytes))
	Expect(err).Should(BeNil())
	WaitForTransactionSuccess(ctx, subnet, txHash)

	teleporterCode, err := subnet.RPCClient.CodeAt(ctx, contractAddress, nil)
	Expect(err).Should(BeNil())
	Expect(len(teleporterCode)).Should(BeNumerically(">", 2)) // 0x is an EOA, contract returns the bytecode
}

func (t TeleporterTestInfo) RelayTeleporterMessage(
	ctx context.Context,
	sourceReceipt *types.Receipt,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	expectSuccess bool,
	fundedKey *ecdsa.PrivateKey,
) *types.Receipt {
	// Fetch the Teleporter message from the logs
	sendEvent, err := GetEventFromLogs(sourceReceipt.Logs, t[source.BlockchainID].TeleporterMessenger.ParseSendCrossChainMessage)
	Expect(err).Should(BeNil())

	signedWarpMessage := ConstructSignedWarpMessage(ctx, sourceReceipt, source, destination)

	// Construct the transaction to send the Warp message to the destination chain
	signedTx := CreateReceiveCrossChainMessageTransaction(
		ctx,
		signedWarpMessage,
		sendEvent.Message.RequiredGasLimit,
		t[source.BlockchainID].TeleporterMessengerAddress,
		fundedKey,
		destination,
	)

	log.Info("Sending transaction to destination chain")
	if !expectSuccess {
		return SendTransactionAndWaitForFailure(ctx, destination, signedTx)
	}

	receipt := SendTransactionAndWaitForSuccess(ctx, destination, signedTx)

	// Check the transaction logs for the ReceiveCrossChainMessage event emitted by the Teleporter contract
	receiveEvent, err := GetEventFromLogs(
		receipt.Logs,
		t[destination.BlockchainID].TeleporterMessenger.ParseReceiveCrossChainMessage,
	)
	Expect(err).Should(BeNil())
	Expect(receiveEvent.SourceBlockchainID[:]).Should(Equal(source.BlockchainID[:]))
	return receipt
}

//
// Deployment utils
//

// Deploys a new version of Teleporter and returns its address
// Does NOT modify the global Teleporter contract address to provide greater testing flexibility.
func DeployNewTeleporterVersion(
	ctx context.Context,
	teleporterInfo TeleporterTestInfo,
	subnet interfaces.SubnetTestInfo,
	fundedKey *ecdsa.PrivateKey,
	teleporterByteCodeFile string,
) common.Address {
	contractCreationGasPrice := (&big.Int{}).Add(deploymentUtils.GetDefaultContractCreationGasPrice(), big.NewInt(1))
	teleporterDeployerTransaction,
		_,
		teleporterDeployerAddress,
		teleporterContractAddress,
		err := deploymentUtils.ConstructKeylessTransaction(
		teleporterByteCodeFile,
		false,
		contractCreationGasPrice,
	)
	Expect(err).Should(BeNil())

	teleporterInfo.DeployTeleporterMessenger(
		ctx,
		subnet,
		teleporterDeployerTransaction,
		teleporterDeployerAddress,
		teleporterContractAddress,
		fundedKey,
	)

	return teleporterContractAddress
}

func DeployTestMessenger(
	ctx context.Context,
	senderKey *ecdsa.PrivateKey,
	teleporterManager common.Address,
	registryAddress common.Address,
	subnet interfaces.SubnetTestInfo,
) (common.Address, *testmessenger.TestMessenger) {
	opts, err := bind.NewKeyedTransactorWithChainID(
		senderKey,
		subnet.EVMChainID,
	)
	Expect(err).Should(BeNil())
	address, tx, exampleMessenger, err := testmessenger.DeployTestMessenger(
		opts,
		subnet.RPCClient,
		registryAddress,
		teleporterManager,
		big.NewInt(1),
	)
	Expect(err).Should(BeNil())

	// Wait for the transaction to be mined
	WaitForTransactionSuccess(ctx, subnet, tx.Hash())

	return address, exampleMessenger
}

//
// Parsing utils
//

func ParseTeleporterMessage(unsignedMessage avalancheWarp.UnsignedMessage) *teleportermessenger.TeleporterMessage {
	addressedPayload, err := payload.ParseAddressedCall(unsignedMessage.Payload)
	Expect(err).Should(BeNil())

	teleporterMessage := teleportermessenger.TeleporterMessage{}
	err = teleporterMessage.Unpack(addressedPayload.Payload)
	Expect(err).Should(BeNil())

	return &teleporterMessage
}

//
// Function call utils
//

func SendAddFeeAmountAndWaitForAcceptance(
	ctx context.Context,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	messageID ids.ID,
	amount *big.Int,
	feeContractAddress common.Address,
	senderKey *ecdsa.PrivateKey,
	transactor *teleportermessenger.TeleporterMessenger,
) *types.Receipt {
	opts, err := bind.NewKeyedTransactorWithChainID(
		senderKey,
		source.EVMChainID,
	)
	Expect(err).Should(BeNil())

	tx, err := transactor.AddFeeAmount(opts, messageID, feeContractAddress, amount)
	Expect(err).Should(BeNil())

	receipt := WaitForTransactionSuccess(ctx, source, tx.Hash())

	addFeeAmountEvent, err := GetEventFromLogs(receipt.Logs, transactor.ParseAddFeeAmount)
	Expect(err).Should(BeNil())
	Expect(addFeeAmountEvent.MessageID[:]).Should(Equal(messageID[:]))

	log.Info("Send AddFeeAmount transaction on source chain",
		"messageID", messageID,
		"sourceChainID", source.BlockchainID,
		"destinationBlockchainID", destination.BlockchainID,
	)

	return receipt
}

func RetryMessageExecutionAndWaitForAcceptance(
	ctx context.Context,
	sourceBlockchainID ids.ID,
	destinationTeleporterMessenger *teleportermessenger.TeleporterMessenger,
	destinationSubnet interfaces.SubnetTestInfo,
	message teleportermessenger.TeleporterMessage,
	senderKey *ecdsa.PrivateKey,
) *types.Receipt {
	opts, err := bind.NewKeyedTransactorWithChainID(senderKey, destinationSubnet.EVMChainID)
	Expect(err).Should(BeNil())

	tx, err := destinationTeleporterMessenger.RetryMessageExecution(opts, sourceBlockchainID, message)
	Expect(err).Should(BeNil())

	return WaitForTransactionSuccess(ctx, destinationSubnet, tx.Hash())
}

func RedeemRelayerRewardsAndConfirm(
	ctx context.Context,
	teleporterMessenger *teleportermessenger.TeleporterMessenger,
	subnet interfaces.SubnetTestInfo,
	feeToken *exampleerc20.ExampleERC20,
	feeTokenAddress common.Address,
	redeemerKey *ecdsa.PrivateKey,
	expectedAmount *big.Int,
) *types.Receipt {
	redeemerAddress := crypto.PubkeyToAddress(redeemerKey.PublicKey)

	// Check the ERC20 balance before redemption
	balanceBeforeRedemption, err := feeToken.BalanceOf(
		&bind.CallOpts{}, redeemerAddress,
	)
	Expect(err).Should(BeNil())

	// Redeem the rewards
	txOpts, err := bind.NewKeyedTransactorWithChainID(
		redeemerKey, subnet.EVMChainID,
	)
	Expect(err).Should(BeNil())
	tx, err := teleporterMessenger.RedeemRelayerRewards(
		txOpts, feeTokenAddress,
	)
	Expect(err).Should(BeNil())
	receipt := WaitForTransactionSuccess(ctx, subnet, tx.Hash())

	// Check that the ERC20 balance was incremented
	balanceAfterRedemption, err := feeToken.BalanceOf(
		&bind.CallOpts{}, redeemerAddress,
	)
	Expect(err).Should(BeNil())
	Expect(balanceAfterRedemption).Should(
		Equal(
			big.NewInt(0).Add(
				balanceBeforeRedemption, expectedAmount,
			),
		),
	)

	// Check that the redeemable rewards amount is now zero.
	updatedRewardAmount, err := teleporterMessenger.CheckRelayerRewardAmount(
		&bind.CallOpts{},
		redeemerAddress,
		feeTokenAddress,
	)
	Expect(err).Should(BeNil())
	Expect(updatedRewardAmount.Cmp(big.NewInt(0))).Should(Equal(0))

	return receipt
}

func SendSpecifiedReceiptsAndWaitForAcceptance(
	ctx context.Context,
	sourceTeleporterMessenger *teleportermessenger.TeleporterMessenger,
	source interfaces.SubnetTestInfo,
	destinationBlockchainID ids.ID,
	messageIDs [][32]byte,
	feeInfo teleportermessenger.TeleporterFeeInfo,
	allowedRelayerAddresses []common.Address,
	senderKey *ecdsa.PrivateKey,
) (*types.Receipt, ids.ID) {
	opts, err := bind.NewKeyedTransactorWithChainID(senderKey, source.EVMChainID)
	Expect(err).Should(BeNil())

	tx, err := sourceTeleporterMessenger.SendSpecifiedReceipts(
		opts, destinationBlockchainID, messageIDs, feeInfo, allowedRelayerAddresses)
	Expect(err).Should(BeNil())

	receipt := WaitForTransactionSuccess(ctx, source, tx.Hash())

	// Check the transaction logs for the SendCrossChainMessage event emitted by the Teleporter contract
	event, err := GetEventFromLogs(receipt.Logs, sourceTeleporterMessenger.ParseSendCrossChainMessage)
	Expect(err).Should(BeNil())
	Expect(event.DestinationBlockchainID[:]).Should(Equal(destinationBlockchainID[:]))

	log.Info("Sending SendSpecifiedReceipts transaction",
		"destinationBlockchainID", destinationBlockchainID,
		"txHash", tx.Hash())

	return receipt, event.MessageID
}

func AddProtocolVersionAndWaitForAcceptance(
	ctx context.Context,
	sourceTeleporterInfo ChainTeleporterInfo,
	subnet interfaces.SubnetTestInfo,
	newTeleporterAddress common.Address,
	senderKey *ecdsa.PrivateKey,
	unsignedMessage *avalancheWarp.UnsignedMessage,
) {
	signedWarpMsg := GetSignedMessage(ctx, subnet, subnet, unsignedMessage.ID())
	log.Info("Got signed warp message", "messageID", signedWarpMsg.ID())

	// Construct tx to add protocol version and send to destination chain
	signedTx := CreateAddProtocolVersionTransaction(
		ctx,
		signedWarpMsg,
		sourceTeleporterInfo.TeleporterRegistryAddress,
		senderKey,
		subnet,
	)

	curLatestVersion, err := sourceTeleporterInfo.TeleporterRegistry.LatestVersion(&bind.CallOpts{})
	Expect(err).Should(BeNil())
	expectedLatestVersion := big.NewInt(curLatestVersion.Int64() + 1)

	// Wait for tx to be accepted, and verify events emitted
	receipt := SendTransactionAndWaitForSuccess(ctx, subnet, signedTx)
	addProtocolVersionEvent, err := GetEventFromLogs(receipt.Logs, sourceTeleporterInfo.TeleporterRegistry.ParseAddProtocolVersion)
	Expect(err).Should(BeNil())
	Expect(addProtocolVersionEvent.Version.Cmp(expectedLatestVersion)).Should(Equal(0))
	Expect(addProtocolVersionEvent.ProtocolAddress).Should(Equal(newTeleporterAddress))

	versionUpdatedEvent, err := GetEventFromLogs(receipt.Logs, sourceTeleporterInfo.TeleporterRegistry.ParseLatestVersionUpdated)
	Expect(err).Should(BeNil())
	Expect(versionUpdatedEvent.OldVersion.Cmp(curLatestVersion)).Should(Equal(0))
	Expect(versionUpdatedEvent.NewVersion.Cmp(expectedLatestVersion)).Should(Equal(0))
}

func SendCrossChainMessageAndWaitForAcceptance(
	ctx context.Context,
	sourceTeleporterMessenger *teleportermessenger.TeleporterMessenger,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	input teleportermessenger.TeleporterMessageInput,
	senderKey *ecdsa.PrivateKey,
) (*types.Receipt, ids.ID) {
	opts, err := bind.NewKeyedTransactorWithChainID(senderKey, source.EVMChainID)
	Expect(err).Should(BeNil())

	// Send a transaction to the Teleporter contract
	tx, err := sourceTeleporterMessenger.SendCrossChainMessage(opts, input)
	Expect(err).Should(BeNil())

	// Wait for the transaction to be accepted
	receipt := WaitForTransactionSuccess(ctx, source, tx.Hash())

	// Check the transaction logs for the SendCrossChainMessage event emitted by the Teleporter contract
	event, err := GetEventFromLogs(receipt.Logs, sourceTeleporterMessenger.ParseSendCrossChainMessage)
	Expect(err).Should(BeNil())

	log.Info("Sending SendCrossChainMessage transaction on source chain",
		"sourceChainID", source.BlockchainID,
		"destinationBlockchainID", destination.BlockchainID,
		"txHash", tx.Hash())

	return receipt, event.MessageID
}

// Returns true if the transaction receipt contains a ReceiptReceived log with the specified messageID
func CheckReceiptReceived(
	receipt *types.Receipt,
	messageID [32]byte,
	transactor *teleportermessenger.TeleporterMessenger,
) bool {
	for _, log := range receipt.Logs {
		event, err := transactor.ParseReceiptReceived(*log)
		if err == nil && bytes.Equal(event.MessageID[:], messageID[:]) {
			return true
		}
	}
	return false
}

func ClearReceiptQueue(
	ctx context.Context,
	teleporterInfo TeleporterTestInfo,
	source interfaces.SubnetTestInfo,
	destination interfaces.SubnetTestInfo,
	fundedKey *ecdsa.PrivateKey,
) {
	outstandReceiptCount := GetOutstandingReceiptCount(teleporterInfo[source.BlockchainID].TeleporterMessenger, destination.BlockchainID)
	for outstandReceiptCount.Cmp(big.NewInt(0)) != 0 {
		log.Info("Emptying receipt queue", "remainingReceipts", outstandReceiptCount.String())
		// Send message from Subnet B to Subnet A to trigger the "regular" method of delivering receipts.
		// The next message from B->A will contain the same receipts that were manually sent in the above steps,
		// but they should not be processed again on Subnet A.
		sendCrossChainMessageInput := teleportermessenger.TeleporterMessageInput{
			DestinationBlockchainID: destination.BlockchainID,
			DestinationAddress:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
			RequiredGasLimit:        big.NewInt(1),
			FeeInfo: teleportermessenger.TeleporterFeeInfo{
				FeeTokenAddress: common.Address{},
				Amount:          big.NewInt(0),
			},
			AllowedRelayerAddresses: []common.Address{},
			Message:                 []byte{1, 2, 3, 4},
		}

		// This message will also have the same receipts as the previous message
		receipt, _ := SendCrossChainMessageAndWaitForAcceptance(
			ctx, teleporterInfo[source.BlockchainID].TeleporterMessenger, source, destination, sendCrossChainMessageInput, fundedKey)

		// Relay message
		teleporterInfo.RelayTeleporterMessage(ctx, receipt, source, destination, true, fundedKey)

		outstandReceiptCount = GetOutstandingReceiptCount(teleporterInfo[source.BlockchainID].TeleporterMessenger, destination.BlockchainID)
	}
	log.Info("Receipt queue emptied")
}

func GetOutstandingReceiptCount(teleporterMessenger *teleportermessenger.TeleporterMessenger, destinationBlockchainID ids.ID) *big.Int {
	size, err := teleporterMessenger.GetReceiptQueueSize(&bind.CallOpts{}, destinationBlockchainID)
	Expect(err).Should(BeNil())
	return size
}

func SendExampleCrossChainMessageAndVerify(
	ctx context.Context,
	teleporterInfo TeleporterTestInfo,
	source interfaces.SubnetTestInfo,
	sourceExampleMessenger *testmessenger.TestMessenger,
	destination interfaces.SubnetTestInfo,
	destExampleMessengerAddress common.Address,
	destExampleMessenger *testmessenger.TestMessenger,
	senderKey *ecdsa.PrivateKey,
	message string,
	expectSuccess bool,
) {
	// Call the example messenger contract on Subnet A
	optsA, err := bind.NewKeyedTransactorWithChainID(senderKey, source.EVMChainID)
	Expect(err).Should(BeNil())
	tx, err := sourceExampleMessenger.SendMessage(
		optsA,
		destination.BlockchainID,
		destExampleMessengerAddress,
		common.BigToAddress(common.Big0),
		big.NewInt(0),
		testmessenger.SendMessageRequiredGas,
		message,
	)
	Expect(err).Should(BeNil())

	// Wait for the transaction to be mined
	receipt := WaitForTransactionSuccess(ctx, source, tx.Hash())

	event, err := GetEventFromLogs(receipt.Logs, teleporterInfo[source.BlockchainID].TeleporterMessenger.ParseSendCrossChainMessage)
	Expect(err).Should(BeNil())
	Expect(event.DestinationBlockchainID[:]).Should(Equal(destination.BlockchainID[:]))

	teleporterMessageID := event.MessageID

	//
	// Relay the message to the destination
	//
	receipt = teleporterInfo.RelayTeleporterMessage(ctx, receipt, source, destination, true, senderKey)

	//
	// Check Teleporter message received on the destination
	//
	delivered, err := teleporterInfo[destination.BlockchainID].TeleporterMessenger.MessageReceived(
		&bind.CallOpts{}, teleporterMessageID,
	)
	Expect(err).Should(BeNil())
	Expect(delivered).Should(BeTrue())

	if expectSuccess {
		// Check that message execution was successful
		messageExecutedEvent, err := GetEventFromLogs(receipt.Logs, teleporterInfo[destination.BlockchainID].TeleporterMessenger.ParseMessageExecuted)
		Expect(err).Should(BeNil())
		Expect(messageExecutedEvent.MessageID[:]).Should(Equal(teleporterMessageID[:]))
	} else {
		// Check that message execution failed
		messageExecutionFailedEvent, err := GetEventFromLogs(
			receipt.Logs,
			teleporterInfo[destination.BlockchainID].TeleporterMessenger.ParseMessageExecutionFailed,
		)
		Expect(err).Should(BeNil())
		Expect(messageExecutionFailedEvent.MessageID[:]).Should(Equal(teleporterMessageID[:]))
	}

	//
	// Verify we received the expected string
	//
	_, currMessage, err := destExampleMessenger.GetCurrentMessage(&bind.CallOpts{}, source.BlockchainID)
	Expect(err).Should(BeNil())
	if expectSuccess {
		Expect(currMessage).Should(Equal(message))
	} else {
		Expect(currMessage).ShouldNot(Equal(message))
	}
}

//
// Transaction utils
//

// Constructs a transaction to call sendCrossChainMessage
// Returns the signed transaction.
func CreateSendCrossChainMessageTransaction(
	ctx context.Context,
	source interfaces.SubnetTestInfo,
	input teleportermessenger.TeleporterMessageInput,
	senderKey *ecdsa.PrivateKey,
	teleporterContractAddress common.Address,
) *types.Transaction {
	data, err := teleportermessenger.PackSendCrossChainMessage(input)
	Expect(err).Should(BeNil())

	gasFeeCap, gasTipCap, nonce := CalculateTxParams(ctx, source, PrivateKeyToAddress(senderKey))

	// Send a transaction to the Teleporter contract
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   source.EVMChainID,
		Nonce:     nonce,
		To:        &teleporterContractAddress,
		Gas:       DefaultTeleporterTransactionGas,
		GasFeeCap: gasFeeCap,
		GasTipCap: gasTipCap,
		Value:     DefaultTeleporterTransactionValue,
		Data:      data,
	})

	return SignTransaction(tx, senderKey, source.EVMChainID)
}

// Constructs a transaction to call receiveCrossChainMessage
// Returns the signed transaction.
func CreateReceiveCrossChainMessageTransaction(
	ctx context.Context,
	signedMessage *avalancheWarp.Message,
	requiredGasLimit *big.Int,
	teleporterContractAddress common.Address,
	senderKey *ecdsa.PrivateKey,
	subnetInfo interfaces.SubnetTestInfo,
) *types.Transaction {
	// Construct the transaction to send the Warp message to the destination chain
	log.Info("Constructing receiveCrossChainMessage transaction for the destination chain")
	numSigners, err := signedMessage.Signature.NumSigners()
	Expect(err).Should(BeNil())

	teleporterMessage := ParseTeleporterMessage(signedMessage.UnsignedMessage)
	gasLimit, err := gasUtils.CalculateReceiveMessageGasLimit(
		numSigners,
		requiredGasLimit,
		len(signedMessage.Bytes()),
		len(signedMessage.Payload),
		len(teleporterMessage.Receipts),
	)
	Expect(err).Should(BeNil())

	callData, err := teleportermessenger.PackReceiveCrossChainMessage(0, PrivateKeyToAddress(senderKey))
	Expect(err).Should(BeNil())

	gasFeeCap, gasTipCap, nonce := CalculateTxParams(ctx, subnetInfo, PrivateKeyToAddress(senderKey))

	destinationTx := predicateutils.NewPredicateTx(
		subnetInfo.EVMChainID,
		nonce,
		&teleporterContractAddress,
		gasLimit,
		gasFeeCap,
		gasTipCap,
		big.NewInt(0),
		callData,
		types.AccessList{},
		warp.ContractAddress,
		signedMessage.Bytes(),
	)

	return SignTransaction(destinationTx, senderKey, subnetInfo.EVMChainID)
}

// Constructs a transaction to call addProtocolVersion
// Returns the signed transaction.
func CreateAddProtocolVersionTransaction(
	ctx context.Context,
	signedMessage *avalancheWarp.Message,
	teleporterRegistryAddress common.Address,
	senderKey *ecdsa.PrivateKey,
	subnetInfo interfaces.SubnetTestInfo,
) *types.Transaction {
	// Construct the transaction to send the Warp message to the destination chain
	log.Info("Constructing addProtocolVersion transaction for the destination chain")

	callData, err := teleporterregistry.PackAddProtocolVersion(0)
	Expect(err).Should(BeNil())

	gasFeeCap, gasTipCap, nonce := CalculateTxParams(ctx, subnetInfo, PrivateKeyToAddress(senderKey))

	destinationTx := predicateutils.NewPredicateTx(
		subnetInfo.EVMChainID,
		nonce,
		&teleporterRegistryAddress,
		500_000,
		gasFeeCap,
		gasTipCap,
		big.NewInt(0),
		callData,
		types.AccessList{},
		warp.ContractAddress,
		signedMessage.Bytes(),
	)

	return SignTransaction(destinationTx, senderKey, subnetInfo.EVMChainID)
}

func CreateExecuteCallPredicateTransaction(
	ctx context.Context,
	signedMessage *avalancheWarp.Message,
	validatorSetSigAddress common.Address,
	senderKey *ecdsa.PrivateKey,
	subnetInfo interfaces.SubnetTestInfo,
) *types.Transaction {
	log.Info("Constructing executeCall transaction for the destination chain")

	callData, err := validatorsetsig.PackExecuteCall(0)
	Expect(err).Should(BeNil())

	gasFeeCap, gasTipCap, nonce := CalculateTxParams(ctx, subnetInfo, PrivateKeyToAddress(senderKey))

	destinationTx := predicateutils.NewPredicateTx(
		subnetInfo.EVMChainID,
		nonce,
		&validatorSetSigAddress,
		500_000,
		gasFeeCap,
		gasTipCap,
		big.NewInt(0),
		callData,
		types.AccessList{},
		warp.ContractAddress,
		signedMessage.Bytes(),
	)
	return SignTransaction(destinationTx, senderKey, subnetInfo.EVMChainID)
}

//
// Off-chain message utils
//

// Creates an Warp message that registers a Teleporter protocol version with TeleporterRegistry.
// Returns the Warp message, as well as the chain config adding the message to the list of approved
// off-chain Warp messages
func InitOffChainMessageChainConfig(
	networkID uint32,
	subnet interfaces.SubnetTestInfo,
	registryAddress common.Address,
	teleporterAddress common.Address,
	version uint64,
) (*avalancheWarp.UnsignedMessage, string) {
	unsignedMessage := CreateOffChainRegistryMessage(networkID, subnet, registryAddress, teleporterregistry.ProtocolRegistryEntry{
		Version:         big.NewInt(int64(version)),
		ProtocolAddress: teleporterAddress,
	})
	log.Info("Adding off-chain message to Warp chain config",
		"messageID", unsignedMessage.ID(),
		"blockchainID", subnet.BlockchainID.String(),
	)

	return unsignedMessage, GetChainConfigWithOffChainMessages([]avalancheWarp.UnsignedMessage{*unsignedMessage})
}

// Creates an off-chain Warp message that registers a Teleporter protocol version with TeleporterRegistry
func CreateOffChainRegistryMessage(
	networkID uint32,
	subnet interfaces.SubnetTestInfo,
	registryAddress common.Address,
	entry teleporterregistry.ProtocolRegistryEntry,
) *avalancheWarp.UnsignedMessage {
	sourceAddress := []byte{}
	payloadBytes, err := teleporterregistry.PackTeleporterRegistryWarpPayload(entry, registryAddress)
	Expect(err).Should(BeNil())

	addressedPayload, err := payload.NewAddressedCall(sourceAddress, payloadBytes)
	Expect(err).Should(BeNil())

	unsignedMessage, err := avalancheWarp.NewUnsignedMessage(
		networkID,
		subnet.BlockchainID,
		addressedPayload.Bytes(),
	)
	Expect(err).Should(BeNil())

	return unsignedMessage
}
