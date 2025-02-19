package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	syncGo "sync"
	"sync/atomic"
	"time"

	"github.com/ElrondNetwork/elrond-go-core/core"
	"github.com/ElrondNetwork/elrond-go-core/core/check"
	"github.com/ElrondNetwork/elrond-go-core/core/partitioning"
	"github.com/ElrondNetwork/elrond-go-core/data/api"
	"github.com/ElrondNetwork/elrond-go-core/data/endProcess"
	"github.com/ElrondNetwork/elrond-go-core/data/esdt"
	"github.com/ElrondNetwork/elrond-go-core/data/transaction"
	disabledSig "github.com/ElrondNetwork/elrond-go-crypto/signing/disabled/singlesig"
	logger "github.com/ElrondNetwork/elrond-go-logger"
	"github.com/ElrondNetwork/elrond-go/common"
	"github.com/ElrondNetwork/elrond-go/consensus"
	"github.com/ElrondNetwork/elrond-go/dataRetriever"
	"github.com/ElrondNetwork/elrond-go/debug"
	"github.com/ElrondNetwork/elrond-go/facade"
	mainFactory "github.com/ElrondNetwork/elrond-go/factory"
	heartbeatData "github.com/ElrondNetwork/elrond-go/heartbeat/data"
	"github.com/ElrondNetwork/elrond-go/node/disabled"
	"github.com/ElrondNetwork/elrond-go/p2p"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/ElrondNetwork/elrond-go/process/dataValidators"
	"github.com/ElrondNetwork/elrond-go/process/factory"
	"github.com/ElrondNetwork/elrond-go/process/smartContract"
	procTx "github.com/ElrondNetwork/elrond-go/process/transaction"
	"github.com/ElrondNetwork/elrond-go/state"
	"github.com/ElrondNetwork/elrond-go/vm"
	"github.com/ElrondNetwork/elrond-go/vm/systemSmartContracts"
	vmcommon "github.com/ElrondNetwork/elrond-vm-common"
)

const (
	// SendTransactionsPipe is the pipe used for sending new transactions
	SendTransactionsPipe = "send transactions pipe"

	// esdtTickerNumChars represents the number of hex-encoded characters of a ticker
	esdtTickerNumChars = 6
)

var log = logger.GetOrCreate("node")
var numSecondsBetweenPrints = 20

var _ facade.NodeHandler = (*Node)(nil)

// Option represents a functional configuration parameter that can operate
//  over the None struct.
type Option func(*Node) error

type filter interface {
	filter(tokenIdentifier string, esdtData *systemSmartContracts.ESDTData) bool
}

// Node is a structure that holds all managed components
type Node struct {
	ctx                 context.Context
	cancelFunc          context.CancelFunc
	initialNodesPubkeys map[uint32][]string
	roundDuration       uint64
	consensusGroupSize  int
	genesisTime         time.Time
	peerDenialEvaluator p2p.PeerDenialEvaluator
	hardforkTrigger     HardforkTrigger

	networkShardingCollector NetworkShardingCollector

	consensusType string

	currentSendingGoRoutines int32
	bootstrapRoundIndex      uint64

	requestedItemsHandler dataRetriever.RequestedItemsHandler

	txSentCounter uint32
	txAcumulator  core.Accumulator

	addressSignatureSize    int
	addressSignatureHexSize int
	validatorSignatureSize  int
	publicKeySize           int

	chanStopNodeProcess chan endProcess.ArgEndProcess

	mutQueryHandlers    syncGo.RWMutex
	queryHandlers       map[string]debug.QueryHandler
	bootstrapComponents mainFactory.BootstrapComponentsHolder
	consensusComponents mainFactory.ConsensusComponentsHolder
	coreComponents      mainFactory.CoreComponentsHolder
	cryptoComponents    mainFactory.CryptoComponentsHolder
	dataComponents      mainFactory.DataComponentsHolder
	heartbeatComponents mainFactory.HeartbeatComponentsHolder
	networkComponents   mainFactory.NetworkComponentsHolder
	processComponents   mainFactory.ProcessComponentsHolder
	stateComponents     mainFactory.StateComponentsHolder
	statusComponents    mainFactory.StatusComponentsHolder

	closableComponents        []mainFactory.Closer
	enableSignTxWithHashEpoch uint32
	isInImportMode            bool
	nodeRedundancyHandler     consensus.NodeRedundancyHandler
}

// ApplyOptions can set up different configurable options of a Node instance
func (n *Node) ApplyOptions(opts ...Option) error {
	for _, opt := range opts {
		err := opt(n)
		if err != nil {
			return errors.New("error applying option: " + err.Error())
		}
	}
	return nil
}

// NewNode creates a new Node instance
func NewNode(opts ...Option) (*Node, error) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	node := &Node{
		ctx:                      ctx,
		cancelFunc:               cancelFunc,
		currentSendingGoRoutines: 0,
		queryHandlers:            make(map[string]debug.QueryHandler),
	}

	node.closableComponents = make([]mainFactory.Closer, 0)

	err := node.ApplyOptions(opts...)
	if err != nil {
		return nil, err
	}

	return node, nil
}

// GetAppStatusHandler will return the current status handler
func (n *Node) GetAppStatusHandler() core.AppStatusHandler {
	return n.coreComponents.StatusHandler()
}

// CreateShardedStores instantiate sharded cachers for Transactions and Headers
func (n *Node) CreateShardedStores() error {
	if check.IfNil(n.processComponents.ShardCoordinator()) {
		return ErrNilShardCoordinator
	}
	if check.IfNil(n.dataComponents.Datapool()) {
		return ErrNilDataPool
	}

	transactionsDataStore := n.dataComponents.Datapool().Transactions()
	headersDataStore := n.dataComponents.Datapool().Headers()

	if transactionsDataStore == nil {
		return errors.New("nil transaction sharded data store")
	}

	if headersDataStore == nil {
		return errors.New("nil header sharded data store")
	}

	return nil
}

// GetConsensusGroupSize returns the configured consensus size
func (n *Node) GetConsensusGroupSize() int {
	return n.consensusGroupSize
}

// GetBalance gets the balance for a specific address
func (n *Node) GetBalance(address string) (*big.Int, error) {
	account, err := n.getAccountHandler(address)
	if err != nil {
		return nil, err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return big.NewInt(0), nil
	}

	return userAccount.GetBalance(), nil
}

// GetUsername gets the username for a specific address
func (n *Node) GetUsername(address string) (string, error) {
	account, err := n.getAccountHandler(address)
	if err != nil {
		return "", err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return "", ErrAccountNotFound
	}

	username := userAccount.GetUserName()
	return string(username), nil
}

// GetAllIssuedESDTs returns all the issued esdt tokens, works only on metachain
func (n *Node) GetAllIssuedESDTs(tokenType string) ([]string, error) {
	if n.processComponents.ShardCoordinator().SelfId() != core.MetachainShardId {
		return nil, ErrMetachainOnlyEndpoint
	}

	account, err := n.getAccountHandlerForPubKey(vm.ESDTSCAddress)
	if err != nil {
		return nil, err
	}

	userAccount, ok := account.(state.UserAccountHandler)
	if !ok {
		return nil, ErrAccountNotFound
	}

	tokens := make([]string, 0)
	if check.IfNil(userAccount.DataTrie()) {
		return tokens, nil
	}

	rootHash, err := userAccount.DataTrie().RootHash()
	if err != nil {
		return nil, err
	}

	chLeaves, err := userAccount.DataTrie().GetAllLeavesOnChannel(rootHash)
	if err != nil {
		return nil, err
	}

	for leaf := range chLeaves {
		tokenName := string(leaf.Key())
		if !strings.Contains(tokenName, "-") {
			continue
		}

		if tokenType == "" {
			tokens = append(tokens, tokenName)
			continue
		}

		esdtToken, okGet := n.getEsdtDataFromLeaf(leaf, userAccount)
		if !okGet {
			continue
		}

		if bytes.Equal(esdtToken.TokenType, []byte(tokenType)) {
			tokens = append(tokens, tokenName)
		}
	}

	return tokens, nil
}

func (n *Node) getEsdtDataFromLeaf(leaf core.KeyValueHolder, userAccount state.UserAccountHandler) (*systemSmartContracts.ESDTData, bool) {
	esdtToken := &systemSmartContracts.ESDTData{}
	suffix := append(leaf.Key(), userAccount.AddressBytes()...)
	value, errVal := leaf.ValueWithoutSuffix(suffix)
	if errVal != nil {
		log.Warn("cannot get value without suffix", "error", errVal, "key", leaf.Key())
		return nil, false
	}

	err := n.coreComponents.InternalMarshalizer().Unmarshal(esdtToken, value)
	if err != nil {
		log.Warn("cannot unmarshal esdt data", "err", err)
		return nil, false
	}

	return esdtToken, true
}

// GetKeyValuePairs returns all the key-value pairs under the address
func (n *Node) GetKeyValuePairs(address string) (map[string]string, error) {
	account, err := n.getAccountHandlerAPIAccounts(address)
	if err != nil {
		return nil, err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return nil, ErrAccountNotFound
	}

	if check.IfNil(userAccount.DataTrie()) {
		return map[string]string{}, nil
	}

	rootHash, err := userAccount.DataTrie().RootHash()
	if err != nil {
		return nil, err
	}

	chLeaves, err := userAccount.DataTrie().GetAllLeavesOnChannel(rootHash)
	if err != nil {
		return nil, err
	}

	mapToReturn := make(map[string]string)
	for leaf := range chLeaves {
		suffix := append(leaf.Key(), userAccount.AddressBytes()...)
		value, errVal := leaf.ValueWithoutSuffix(suffix)
		if errVal != nil {
			log.Warn("cannot get value without suffix", "error", errVal, "key", leaf.Key())
			continue
		}

		mapToReturn[hex.EncodeToString(leaf.Key())] = hex.EncodeToString(value)
	}

	return mapToReturn, nil
}

// GetValueForKey will return the value for a key from a given account
func (n *Node) GetValueForKey(address string, key string) (string, error) {
	keyBytes, err := hex.DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("invalid key: %w", err)
	}

	account, err := n.getAccountHandler(address)
	if err != nil {
		return "", err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return "", ErrAccountNotFound
	}

	valueBytes, err := userAccount.DataTrieTracker().RetrieveValue(keyBytes)
	if err != nil {
		return "", fmt.Errorf("fetching value error: %w", err)
	}

	return hex.EncodeToString(valueBytes), nil
}

// GetESDTData returns the esdt balance and properties from a given account
func (n *Node) GetESDTData(address, tokenID string, nonce uint64) (*esdt.ESDigitalToken, error) {
	account, err := n.getAccountHandler(address)
	if err != nil {
		return nil, err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return nil, ErrAccountNotFound
	}

	esdtToken := &esdt.ESDigitalToken{Value: big.NewInt(0)}
	tokenKey := core.ElrondProtectedKeyPrefix + core.ESDTKeyIdentifier + tokenID
	if nonce > 0 {
		tokenKey += string(big.NewInt(0).SetUint64(nonce).Bytes())
	}

	dataBytes, err := userAccount.DataTrieTracker().RetrieveValue([]byte(tokenKey))
	if err != nil || len(dataBytes) == 0 {
		return esdtToken, nil
	}

	err = n.coreComponents.InternalMarshalizer().Unmarshal(esdtToken, dataBytes)
	if err != nil {
		return nil, err
	}

	if esdtToken.TokenMetaData != nil {
		esdtToken.TokenMetaData.Creator = []byte(n.coreComponents.AddressPubKeyConverter().Encode(esdtToken.TokenMetaData.Creator))
	}

	return esdtToken, nil
}

func (n *Node) getTokensIDsWithFilter(
	f filter,
) ([]string, error) {
	if n.processComponents.ShardCoordinator().SelfId() != core.MetachainShardId {
		return nil, ErrMetachainOnlyEndpoint
	}

	account, err := n.getAccountHandlerForPubKey(vm.ESDTSCAddress)
	if err != nil {
		return nil, err
	}

	userAccount, ok := account.(state.UserAccountHandler)
	if !ok {
		return nil, ErrAccountNotFound
	}

	tokens := make([]string, 0)
	if check.IfNil(userAccount.DataTrie()) {
		return tokens, nil
	}

	rootHash, err := userAccount.DataTrie().RootHash()
	if err != nil {
		return nil, err
	}

	chLeaves, err := userAccount.DataTrie().GetAllLeavesOnChannel(rootHash)
	if err != nil {
		return nil, err
	}

	for leaf := range chLeaves {
		tokenIdentifier := string(leaf.Key())
		if !strings.Contains(tokenIdentifier, "-") {
			continue
		}

		esdtToken, okGet := n.getEsdtDataFromLeaf(leaf, userAccount)
		if !okGet {
			continue
		}

		if f.filter(tokenIdentifier, esdtToken) {
			tokens = append(tokens, tokenIdentifier)
		}
	}

	return tokens, nil
}

// GetNFTTokenIDsRegisteredByAddress returns all the token identifiers for semi or non fungible tokens registered by the address
func (n *Node) GetNFTTokenIDsRegisteredByAddress(address string) ([]string, error) {
	addressBytes, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return nil, err
	}

	f := &getRegisteredNftsFilter{
		addressBytes: addressBytes,
	}
	return n.getTokensIDsWithFilter(f)
}

// GetESDTsWithRole returns all the tokens with the given role for the given address
func (n *Node) GetESDTsWithRole(address string, role string) ([]string, error) {
	if !core.IsValidESDTRole(role) {
		return nil, ErrInvalidESDTRole
	}

	addressBytes, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return nil, err
	}

	f := &getTokensWithRoleFilter{
		addressBytes: addressBytes,
		role:         role,
	}
	return n.getTokensIDsWithFilter(f)
}

// GetESDTsRoles returns all the tokens identifiers and roles for the given address
func (n *Node) GetESDTsRoles(address string) (map[string][]string, error) {
	addressBytes, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return nil, err
	}

	tokensRoles := make(map[string][]string)

	f := &getAllTokensRolesFilter{
		addressBytes: addressBytes,
		outputRoles:  tokensRoles,
	}
	_, err = n.getTokensIDsWithFilter(f)
	if err != nil {
		return nil, err
	}

	return tokensRoles, nil
}

// GetAllESDTTokens returns all the ESDTs that the given address interacted with
func (n *Node) GetAllESDTTokens(address string) (map[string]*esdt.ESDigitalToken, error) {
	account, err := n.getAccountHandlerAPIAccounts(address)
	if err != nil {
		return nil, err
	}

	userAccount, ok := n.castAccountToUserAccount(account)
	if !ok {
		return nil, ErrAccountNotFound
	}

	allESDTs := make(map[string]*esdt.ESDigitalToken)
	if check.IfNil(userAccount.DataTrie()) {
		return allESDTs, nil
	}

	esdtPrefix := []byte(core.ElrondProtectedKeyPrefix + core.ESDTKeyIdentifier)
	lenESDTPrefix := len(esdtPrefix)

	rootHash, err := userAccount.DataTrie().RootHash()
	if err != nil {
		return nil, err
	}

	chLeaves, err := userAccount.DataTrie().GetAllLeavesOnChannel(rootHash)
	if err != nil {
		return nil, err
	}
	for leaf := range chLeaves {
		if !bytes.HasPrefix(leaf.Key(), esdtPrefix) {
			continue
		}

		tokenName := string(leaf.Key()[lenESDTPrefix:])
		esdtToken := &esdt.ESDigitalToken{Value: big.NewInt(0)}

		suffix := append(leaf.Key(), userAccount.AddressBytes()...)
		value, errVal := leaf.ValueWithoutSuffix(suffix)
		if errVal != nil {
			log.Warn("cannot get value without suffix", "error", errVal, "key", leaf.Key())
			continue
		}

		err = n.coreComponents.InternalMarshalizer().Unmarshal(esdtToken, value)
		if err != nil {
			log.Warn("cannot unmarshal", "token name", tokenName, "err", err)
			continue
		}

		if esdtToken.TokenMetaData != nil {
			esdtToken.TokenMetaData.Creator = []byte(n.coreComponents.AddressPubKeyConverter().Encode(esdtToken.TokenMetaData.Creator))
			tokenName = adjustNftTokenIdentifier(tokenName, esdtToken.TokenMetaData.Nonce)
		}

		allESDTs[tokenName] = esdtToken
	}

	return allESDTs, nil
}

func adjustNftTokenIdentifier(token string, nonce uint64) string {
	splitToken := strings.Split(token, "-")
	if len(splitToken) < 2 {
		return token
	}

	if len(splitToken[1]) < esdtTickerNumChars {
		return token
	}

	nonceBytes := big.NewInt(0).SetUint64(nonce).Bytes()
	formattedTokenIdentifier := fmt.Sprintf("%s-%s-%s",
		splitToken[0],
		splitToken[1][:esdtTickerNumChars],
		hex.EncodeToString(nonceBytes))

	return formattedTokenIdentifier
}

func (n *Node) getAccountHandler(address string) (vmcommon.AccountHandler, error) {
	if check.IfNil(n.coreComponents.AddressPubKeyConverter()) || check.IfNil(n.stateComponents.AccountsAdapter()) {
		return nil, errors.New("initialize AccountsAdapter and PubkeyConverter first")
	}

	addr, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return nil, errors.New("invalid address, could not decode from: " + err.Error())
	}
	return n.stateComponents.AccountsAdapter().GetExistingAccount(addr)
}

func (n *Node) getAccountHandlerAPIAccounts(address string) (vmcommon.AccountHandler, error) {
	componentsNotInitialized := check.IfNil(n.coreComponents.AddressPubKeyConverter()) ||
		check.IfNil(n.stateComponents.AccountsAdapterAPI()) ||
		check.IfNil(n.dataComponents.Blockchain())
	if componentsNotInitialized {
		return nil, errors.New("initialize AccountsAdapterAPI, PubkeyConverter and Blockchain first")
	}

	addr, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return nil, errors.New("invalid address, could not decode from: " + err.Error())
	}

	return n.getAccountHandlerForPubKey(addr)
}

func (n *Node) getAccountHandlerForPubKey(address []byte) (vmcommon.AccountHandler, error) {
	blockHeader := n.dataComponents.Blockchain().GetCurrentBlockHeader()
	if check.IfNil(blockHeader) {
		return nil, ErrNilBlockHeader
	}

	err := n.stateComponents.AccountsAdapterAPI().RecreateTrie(blockHeader.GetRootHash())
	if err != nil {
		return nil, err
	}

	return n.stateComponents.AccountsAdapterAPI().GetExistingAccount(address)
}

func (n *Node) castAccountToUserAccount(ah vmcommon.AccountHandler) (state.UserAccountHandler, bool) {
	if check.IfNil(ah) {
		return nil, false
	}

	account, ok := ah.(state.UserAccountHandler)
	return account, ok
}

// SendBulkTransactions sends the provided transactions as a bulk, optimizing transfer between nodes
func (n *Node) SendBulkTransactions(txs []*transaction.Transaction) (uint64, error) {
	if len(txs) == 0 {
		return 0, ErrNoTxToProcess
	}

	n.addTransactionsToSendPipe(txs)

	return uint64(len(txs)), nil
}

func (n *Node) addTransactionsToSendPipe(txs []*transaction.Transaction) {
	if check.IfNil(n.txAcumulator) {
		log.Error("node has a nil tx accumulator instance")
		return
	}

	for _, tx := range txs {
		n.txAcumulator.AddData(tx)
	}
}

func (n *Node) sendFromTxAccumulator(ctx context.Context) {
	outputChannel := n.txAcumulator.OutputChannel()

	for {
		select {
		case objs := <-outputChannel:
			{
				if len(objs) == 0 {
					break
				}

				txs := make([]*transaction.Transaction, 0, len(objs))
				for _, obj := range objs {
					tx, ok := obj.(*transaction.Transaction)
					if !ok {
						continue
					}

					txs = append(txs, tx)
				}

				atomic.AddUint32(&n.txSentCounter, uint32(len(txs)))

				n.sendBulkTransactions(txs)
			}
		case <-ctx.Done():
			return
		}
	}
}

// printTxSentCounter prints the peak transaction counter from a time frame of about 'numSecondsBetweenPrints' seconds
// if this peak value is 0 (no transaction was sent through the REST API interface), the print will not be done
// the peak counter resets after each print. There is also a total number of transactions sent to p2p
// TODO make this function testable. Refactor if necessary.
func (n *Node) printTxSentCounter(ctx context.Context) {
	maxTxCounter := uint32(0)
	totalTxCounter := uint64(0)
	counterSeconds := 0

	for {
		select {
		case <-time.After(time.Second):
			txSent := atomic.SwapUint32(&n.txSentCounter, 0)
			if txSent > maxTxCounter {
				maxTxCounter = txSent
			}
			totalTxCounter += uint64(txSent)

			counterSeconds++
			if counterSeconds > numSecondsBetweenPrints {
				counterSeconds = 0

				if maxTxCounter > 0 {
					log.Info("sent transactions on network",
						"max/sec", maxTxCounter,
						"total", totalTxCounter,
					)
				}
				maxTxCounter = 0
			}
		case <-ctx.Done():
			return
		}
	}
}

// sendBulkTransactions sends the provided transactions as a bulk, optimizing transfer between nodes
func (n *Node) sendBulkTransactions(txs []*transaction.Transaction) {
	transactionsByShards := make(map[uint32][][]byte)
	log.Trace("node.sendBulkTransactions sending txs",
		"num", len(txs),
	)

	for _, tx := range txs {
		senderShardId := n.processComponents.ShardCoordinator().ComputeId(tx.SndAddr)

		marshalizedTx, err := n.coreComponents.InternalMarshalizer().Marshal(tx)
		if err != nil {
			log.Warn("node.sendBulkTransactions",
				"marshalizer error", err,
			)
			continue
		}

		transactionsByShards[senderShardId] = append(transactionsByShards[senderShardId], marshalizedTx)
	}

	numOfSentTxs := uint64(0)
	for shardId, txsForShard := range transactionsByShards {
		err := n.sendBulkTransactionsFromShard(txsForShard, shardId)
		if err != nil {
			log.Debug("sendBulkTransactionsFromShard", "error", err.Error())
		} else {
			numOfSentTxs += uint64(len(txsForShard))
		}
	}
}

// ValidateTransaction will validate a transaction
func (n *Node) ValidateTransaction(tx *transaction.Transaction) error {
	err := n.checkSenderIsInShard(tx)
	if err != nil {
		return err
	}

	txValidator, intTx, err := n.commonTransactionValidation(tx, n.processComponents.WhiteListerVerifiedTxs(), n.processComponents.WhiteListHandler(), true)
	if err != nil {
		return err
	}

	return txValidator.CheckTxValidity(intTx)
}

// ValidateTransactionForSimulation will validate a transaction for use in transaction simulation process
func (n *Node) ValidateTransactionForSimulation(tx *transaction.Transaction, checkSignature bool) error {
	disabledWhiteListHandler := disabled.NewDisabledWhiteListDataVerifier()
	txValidator, intTx, err := n.commonTransactionValidation(tx, disabledWhiteListHandler, disabledWhiteListHandler, checkSignature)
	if err != nil {
		return err
	}

	err = txValidator.CheckTxValidity(intTx)
	if errors.Is(err, process.ErrAccountNotFound) {
		// we allow the broadcast of provided transaction even if that transaction is not targeted on the current shard
		return nil
	}

	return err
}

func (n *Node) commonTransactionValidation(
	tx *transaction.Transaction,
	whiteListerVerifiedTxs process.WhiteListHandler,
	whiteListRequest process.WhiteListHandler,
	checkSignature bool,
) (process.TxValidator, process.TxValidatorHandler, error) {
	txValidator, err := dataValidators.NewTxValidator(
		n.stateComponents.AccountsAdapter(),
		n.processComponents.ShardCoordinator(),
		whiteListRequest,
		n.coreComponents.AddressPubKeyConverter(),
		common.MaxTxNonceDeltaAllowed,
	)
	if err != nil {
		log.Warn("node.ValidateTransaction: can not instantiate a TxValidator",
			"error", err)
		return nil, nil, err
	}

	marshalizedTx, err := n.coreComponents.InternalMarshalizer().Marshal(tx)
	if err != nil {
		return nil, nil, err
	}

	currentEpoch := n.coreComponents.EpochNotifier().CurrentEpoch()
	enableSignWithTxHash := currentEpoch >= n.enableSignTxWithHashEpoch

	txSingleSigner := n.cryptoComponents.TxSingleSigner()
	if !checkSignature {
		txSingleSigner = &disabledSig.DisabledSingleSig{}
	}

	argumentParser := smartContract.NewArgumentParser()
	intTx, err := procTx.NewInterceptedTransaction(
		marshalizedTx,
		n.coreComponents.InternalMarshalizer(),
		n.coreComponents.TxMarshalizer(),
		n.coreComponents.Hasher(),
		n.cryptoComponents.TxSignKeyGen(),
		txSingleSigner,
		n.coreComponents.AddressPubKeyConverter(),
		n.processComponents.ShardCoordinator(),
		n.coreComponents.EconomicsData(),
		whiteListerVerifiedTxs,
		argumentParser,
		[]byte(n.coreComponents.ChainID()),
		enableSignWithTxHash,
		n.coreComponents.TxSignHasher(),
		n.coreComponents.TxVersionChecker(),
	)
	if err != nil {
		return nil, nil, err
	}

	err = intTx.CheckValidity()
	if err != nil {
		return nil, nil, err
	}

	return txValidator, intTx, nil
}

func (n *Node) checkSenderIsInShard(tx *transaction.Transaction) error {
	shardCoordinator := n.bootstrapComponents.ShardCoordinator()
	senderShardID := shardCoordinator.ComputeId(tx.SndAddr)
	if senderShardID != shardCoordinator.SelfId() {
		return fmt.Errorf("%w, tx sender shard ID: %d, node's shard ID %d",
			ErrDifferentSenderShardId, senderShardID, shardCoordinator.SelfId())
	}

	return nil
}

func (n *Node) sendBulkTransactionsFromShard(transactions [][]byte, senderShardId uint32) error {
	dataPacker, err := partitioning.NewSimpleDataPacker(n.coreComponents.InternalMarshalizer())
	if err != nil {
		return err
	}

	// the topic identifier is made of the current shard id and sender's shard id
	identifier := factory.TransactionTopic + n.processComponents.ShardCoordinator().CommunicationIdentifier(senderShardId)

	packets, err := dataPacker.PackDataInChunks(transactions, common.MaxBulkTransactionSize)
	if err != nil {
		return err
	}

	atomic.AddInt32(&n.currentSendingGoRoutines, int32(len(packets)))
	for _, buff := range packets {
		go func(bufferToSend []byte) {
			log.Trace("node.sendBulkTransactionsFromShard",
				"topic", identifier,
				"size", len(bufferToSend),
			)
			err = n.networkComponents.NetworkMessenger().BroadcastOnChannelBlocking(
				SendTransactionsPipe,
				identifier,
				bufferToSend,
			)
			if err != nil {
				log.Debug("node.BroadcastOnChannelBlocking", "error", err.Error())
			}

			atomic.AddInt32(&n.currentSendingGoRoutines, -1)
		}(buff)
	}

	return nil
}

// CreateTransaction will return a transaction from all the required fields
func (n *Node) CreateTransaction(
	nonce uint64,
	value string,
	receiver string,
	receiverUsername []byte,
	sender string,
	senderUsername []byte,
	gasPrice uint64,
	gasLimit uint64,
	dataField []byte,
	signatureHex string,
	chainID string,
	version uint32,
	options uint32,
) (*transaction.Transaction, []byte, error) {
	if version == 0 {
		return nil, nil, ErrInvalidTransactionVersion
	}
	if chainID == "" || len(chainID) > len(n.coreComponents.ChainID()) {
		return nil, nil, ErrInvalidChainIDInTransaction
	}
	addrPubKeyConverter := n.coreComponents.AddressPubKeyConverter()
	if check.IfNil(addrPubKeyConverter) {
		return nil, nil, ErrNilPubkeyConverter
	}
	if check.IfNil(n.stateComponents.AccountsAdapter()) {
		return nil, nil, ErrNilAccountsAdapter
	}
	if len(signatureHex) > n.addressSignatureHexSize {
		return nil, nil, ErrInvalidSignatureLength
	}
	if uint32(len(receiver)) > n.coreComponents.EncodedAddressLen() {
		return nil, nil, fmt.Errorf("%w for receiver", ErrInvalidAddressLength)
	}
	if uint32(len(sender)) > n.coreComponents.EncodedAddressLen() {
		return nil, nil, fmt.Errorf("%w for sender", ErrInvalidAddressLength)
	}
	if len(senderUsername) > core.MaxUserNameLength {
		return nil, nil, ErrInvalidSenderUsernameLength
	}
	if len(receiverUsername) > core.MaxUserNameLength {
		return nil, nil, ErrInvalidReceiverUsernameLength
	}
	if len(dataField) > core.MegabyteSize {
		return nil, nil, ErrDataFieldTooBig
	}

	receiverAddress, err := addrPubKeyConverter.Decode(receiver)
	if err != nil {
		return nil, nil, errors.New("could not create receiver address from provided param")
	}

	senderAddress, err := addrPubKeyConverter.Decode(sender)
	if err != nil {
		return nil, nil, errors.New("could not create sender address from provided param")
	}

	signatureBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return nil, nil, errors.New("could not fetch signature bytes")
	}

	if len(value) > len(n.coreComponents.EconomicsData().GenesisTotalSupply().String())+1 {
		return nil, nil, ErrTransactionValueLengthTooBig
	}

	valAsBigInt, ok := big.NewInt(0).SetString(value, 10)
	if !ok {
		return nil, nil, ErrInvalidValue
	}

	tx := &transaction.Transaction{
		Nonce:       nonce,
		Value:       valAsBigInt,
		RcvAddr:     receiverAddress,
		RcvUserName: receiverUsername,
		SndAddr:     senderAddress,
		SndUserName: senderUsername,
		GasPrice:    gasPrice,
		GasLimit:    gasLimit,
		Data:        dataField,
		Signature:   signatureBytes,
		ChainID:     []byte(chainID),
		Version:     version,
		Options:     options,
	}

	var txHash []byte
	txHash, err = core.CalculateHash(n.coreComponents.InternalMarshalizer(), n.coreComponents.Hasher(), tx)
	if err != nil {
		return nil, nil, err
	}

	return tx, txHash, nil
}

// GetAccount will return account details for a given address
func (n *Node) GetAccount(address string) (api.AccountResponse, error) {
	if check.IfNil(n.coreComponents.AddressPubKeyConverter()) {
		return api.AccountResponse{}, ErrNilPubkeyConverter
	}
	if check.IfNil(n.stateComponents.AccountsAdapter()) {
		return api.AccountResponse{}, ErrNilAccountsAdapter
	}

	addr, err := n.coreComponents.AddressPubKeyConverter().Decode(address)
	if err != nil {
		return api.AccountResponse{}, err
	}

	accWrp, err := n.stateComponents.AccountsAdapter().GetExistingAccount(addr)
	if err != nil {
		if err == state.ErrAccNotFound {
			return api.AccountResponse{
				Address:         address,
				Balance:         "0",
				DeveloperReward: "0",
			}, nil
		}
		return api.AccountResponse{}, errors.New("could not fetch sender address from provided param: " + err.Error())
	}

	account, ok := accWrp.(state.UserAccountHandler)
	if !ok {
		return api.AccountResponse{}, errors.New("account is not of type with balance and nonce")
	}

	ownerAddress := ""
	if len(account.GetOwnerAddress()) > 0 {
		addressPubkeyConverter := n.coreComponents.AddressPubKeyConverter()
		ownerAddress = addressPubkeyConverter.Encode(account.GetOwnerAddress())
	}

	return api.AccountResponse{
		Address:         address,
		Nonce:           account.GetNonce(),
		Balance:         account.GetBalance().String(),
		Username:        string(account.GetUserName()),
		CodeHash:        account.GetCodeHash(),
		RootHash:        account.GetRootHash(),
		CodeMetadata:    account.GetCodeMetadata(),
		DeveloperReward: account.GetDeveloperReward().String(),
		OwnerAddress:    ownerAddress,
	}, nil
}

// GetCode returns the code for the given code hash
func (n *Node) GetCode(codeHash []byte) []byte {
	return n.stateComponents.AccountsAdapter().GetCode(codeHash)
}

// GetHeartbeats returns the heartbeat status for each public key defined in genesis.json
func (n *Node) GetHeartbeats() []heartbeatData.PubKeyHeartbeat {
	if check.IfNil(n.heartbeatComponents) {
		return make([]heartbeatData.PubKeyHeartbeat, 0)
	}
	mon := n.heartbeatComponents.Monitor()
	if check.IfNil(mon) {
		return make([]heartbeatData.PubKeyHeartbeat, 0)
	}

	return mon.GetHeartbeats()
}

// ValidatorStatisticsApi will return the statistics for all the validators from the initial nodes pub keys
func (n *Node) ValidatorStatisticsApi() (map[string]*state.ValidatorApiResponse, error) {
	return n.processComponents.ValidatorsProvider().GetLatestValidators(), nil
}

// DirectTrigger will start the hardfork trigger
func (n *Node) DirectTrigger(epoch uint32, withEarlyEndOfEpoch bool) error {
	return n.hardforkTrigger.Trigger(epoch, withEarlyEndOfEpoch)
}

// IsSelfTrigger returns true if the trigger's registered public key matches the self public key
func (n *Node) IsSelfTrigger() bool {
	return n.hardforkTrigger.IsSelfTrigger()
}

// EncodeAddressPubkey will encode the provided address public key bytes to string
func (n *Node) EncodeAddressPubkey(pk []byte) (string, error) {
	if n.coreComponents.AddressPubKeyConverter() == nil {
		return "", fmt.Errorf("%w for addressPubkeyConverter", ErrNilPubkeyConverter)
	}

	return n.coreComponents.AddressPubKeyConverter().Encode(pk), nil
}

// DecodeAddressPubkey will try to decode the provided address public key string
func (n *Node) DecodeAddressPubkey(pk string) ([]byte, error) {
	if n.coreComponents.AddressPubKeyConverter() == nil {
		return nil, fmt.Errorf("%w for addressPubkeyConverter", ErrNilPubkeyConverter)
	}

	return n.coreComponents.AddressPubKeyConverter().Decode(pk)
}

// AddQueryHandler adds a query handler in cache
func (n *Node) AddQueryHandler(name string, handler debug.QueryHandler) error {
	if check.IfNil(handler) {
		return ErrNilQueryHandler
	}
	if len(name) == 0 {
		return ErrEmptyQueryHandlerName
	}

	n.mutQueryHandlers.Lock()
	defer n.mutQueryHandlers.Unlock()

	_, ok := n.queryHandlers[name]
	if ok {
		return fmt.Errorf("%w with name %s", ErrQueryHandlerAlreadyExists, name)
	}

	n.queryHandlers[name] = handler

	return nil
}

// GetQueryHandler returns the query handler if existing
func (n *Node) GetQueryHandler(name string) (debug.QueryHandler, error) {
	n.mutQueryHandlers.RLock()
	defer n.mutQueryHandlers.RUnlock()

	qh, ok := n.queryHandlers[name]
	if !ok || check.IfNil(qh) {
		return nil, fmt.Errorf("%w for name %s", ErrNilQueryHandler, name)
	}

	return qh, nil
}

// GetPeerInfo returns information about a peer id
func (n *Node) GetPeerInfo(pid string) ([]core.QueryP2PPeerInfo, error) {
	peers := n.networkComponents.NetworkMessenger().Peers()
	pidsFound := make([]core.PeerID, 0)
	for _, p := range peers {
		if strings.Contains(p.Pretty(), pid) {
			pidsFound = append(pidsFound, p)
		}
	}

	if len(pidsFound) == 0 {
		return nil, fmt.Errorf("%w for provided peer %s", ErrUnknownPeerID, pid)
	}

	sort.Slice(pidsFound, func(i, j int) bool {
		return pidsFound[i].Pretty() < pidsFound[j].Pretty()
	})

	peerInfoSlice := make([]core.QueryP2PPeerInfo, 0, len(pidsFound))
	for _, p := range pidsFound {
		pidInfo := n.createPidInfo(p)
		peerInfoSlice = append(peerInfoSlice, pidInfo)
	}

	return peerInfoSlice, nil
}

// GetHardforkTrigger returns the hardfork trigger
func (n *Node) GetHardforkTrigger() HardforkTrigger {
	return n.hardforkTrigger
}

// GetCoreComponents returns the core components
func (n *Node) GetCoreComponents() mainFactory.CoreComponentsHolder {
	return n.coreComponents
}

// GetCryptoComponents returns the crypto components
func (n *Node) GetCryptoComponents() mainFactory.CryptoComponentsHolder {
	return n.cryptoComponents
}

// GetConsensusComponents returns the consensus components
func (n *Node) GetConsensusComponents() mainFactory.ConsensusComponentsHolder {
	return n.consensusComponents
}

// GetBootstrapComponents returns the bootstrap components
func (n *Node) GetBootstrapComponents() mainFactory.BootstrapComponentsHolder {
	return n.bootstrapComponents
}

// GetDataComponents returns the data components
func (n *Node) GetDataComponents() mainFactory.DataComponentsHolder {
	return n.dataComponents
}

// GetHeartbeatComponents returns the heartbeat components
func (n *Node) GetHeartbeatComponents() mainFactory.HeartbeatComponentsHolder {
	return n.heartbeatComponents
}

// GetNetworkComponents returns the network components
func (n *Node) GetNetworkComponents() mainFactory.NetworkComponentsHolder {
	return n.networkComponents
}

// GetProcessComponents returns the process components
func (n *Node) GetProcessComponents() mainFactory.ProcessComponentsHolder {
	return n.processComponents
}

// GetStateComponents returns the state components
func (n *Node) GetStateComponents() mainFactory.StateComponentsHolder {
	return n.stateComponents
}

// GetStatusComponents returns the status components
func (n *Node) GetStatusComponents() mainFactory.StatusComponentsHolder {
	return n.statusComponents
}

func (n *Node) createPidInfo(p core.PeerID) core.QueryP2PPeerInfo {
	result := core.QueryP2PPeerInfo{
		Pid:           p.Pretty(),
		Addresses:     n.networkComponents.NetworkMessenger().PeerAddresses(p),
		IsBlacklisted: n.peerDenialEvaluator.IsDenied(p),
	}

	peerInfo := n.networkShardingCollector.GetPeerInfo(p)
	result.PeerType = peerInfo.PeerType.String()
	result.PeerSubType = peerInfo.PeerSubType.String()
	if len(peerInfo.PkBytes) == 0 {
		result.Pk = ""
	} else {
		result.Pk = n.coreComponents.ValidatorPubKeyConverter().Encode(peerInfo.PkBytes)
	}

	return result
}

// Close closes all underlying components
func (n *Node) Close() error {
	n.cancelFunc()

	for _, qh := range n.queryHandlers {
		log.LogIfError(qh.Close())
	}

	var closeError error = nil
	log.Debug("closing all managed components")
	for i := len(n.closableComponents) - 1; i >= 0; i-- {
		managedComponent := n.closableComponents[i]
		componentName := n.getClosableComponentName(managedComponent, i)
		log.Debug("closing", "managedComponent", componentName)
		err := managedComponent.Close()
		if err != nil {
			if closeError == nil {
				closeError = ErrNodeCloseFailed
			}
			closeError = fmt.Errorf("%w, err: %s", closeError, err.Error())
		}
	}

	time.Sleep(time.Second * 5)

	return closeError
}

func (n *Node) getClosableComponentName(component mainFactory.Closer, index int) string {
	componentStringer, ok := component.(fmt.Stringer)
	if !ok {
		return fmt.Sprintf("n.closableComponents[%d] - %v", index, component)
	}

	return componentStringer.String()
}

// IsInImportMode returns true if the node is in import mode
func (n *Node) IsInImportMode() bool {
	return n.isInImportMode
}

// IsInterfaceNil returns true if there is no value under the interface
func (n *Node) IsInterfaceNil() bool {
	return n == nil
}
