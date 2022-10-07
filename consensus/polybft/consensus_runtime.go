package polybft

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/0xPolygon/pbft-consensus"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/bitmap"
	bls "github.com/0xPolygon/polygon-edge/consensus/polybft/signer"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/wallet"
	"github.com/0xPolygon/polygon-edge/types"
	hcf "github.com/hashicorp/go-hclog"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/abi"
)

const (
	eventsBufferSize   = 10
	stateFileName      = "consensusState.db"
	uptimeLookbackSize = 2 // number of blocks to calculate uptime from the previous epoch
)

var (
	// state sync metrics
	// TO DO Nemanja- what to do with metrics
	// totalStateSyncsMeter = metrics.NewRegisteredMeter("consensus/bridge/stateSyncsTotal", nil)

	// errNotAValidator represents "node is not a validator" error message
	errNotAValidator = errors.New("node is not a validator")
	// errQuorumNotReached represents "quorum not reached for commitment message" error message
	errQuorumNotReached = errors.New("quorum not reached for commitment message")
)

// Transport is an abstraction of network layer
type Transport interface {
	Gossip(message interface{})
}

// epochMetadata is the static info for epoch currently being processed
type epochMetadata struct {
	// Number is the number of the epoch
	Number uint64

	// LastCheckpoint is the last epoch that was checkpointed, for now it is epoch-1.
	LastCheckpoint uint64

	// CheckpointProposer is the validator that has to send the checkpoint, assume it is static for now.
	CheckpointProposer pbft.NodeID

	// Blocks is the list of blocks that we have to checkpoint in rootchain
	Blocks []*types.Block

	// Validators is the set of validators for the epoch
	Validators AccountSet

	// Commitment built in the current epoch
	Commitment *Commitment
}

// runtimeConfig is a struct that holds configuration data for given consensus runtime
type runtimeConfig struct {
	PolyBFTConfig  *PolyBFTConfig
	DataDir        string
	Transport      Transport
	Key            *wallet.Key
	State          *State
	blockchain     blockchainBackend
	polybftBackend polybftBackend
}

// consensusRuntime is a struct that provides consensus runtime features like epoch, state and event management
type consensusRuntime struct {
	// config represents wrapper around required parameters which are received from the outside
	config *runtimeConfig

	// state is reference to the struct which encapsulates bridge events persistence logic
	state *State

	// eventTracker is a reference to the log event tracker
	eventTracker *eventTracker

	// epoch is the metadata for the current epoch
	epoch *epochMetadata

	// lock is a lock to access 'epoch'
	lock sync.Mutex

	// lastBuiltBlock is the header of the last processed block
	lastBuiltBlock *types.Header

	// activeValidatorFlag indicates whether the given node is amongst currently active validator set
	activeValidatorFlag uint32

	logger hcf.Logger
}

// newConsensusRuntime creates and starts a new consensus runtime instance with event tracking
func newConsensusRuntime(log hcf.Logger, config *runtimeConfig) (*consensusRuntime, error) {
	runtime := &consensusRuntime{
		state:  config.State,
		config: config,
		logger: log.Named("consensus_runtime"),
	}

	if runtime.IsBridgeEnabled() {
		err := runtime.startEventTracker()
		if err != nil {
			return nil, err
		}
	}

	return runtime, nil
}

// getEpoch returns current epochMetadata in a thread-safe manner.
func (c *consensusRuntime) getEpoch() *epochMetadata {
	c.lock.Lock()
	epoch := c.epoch
	c.lock.Unlock()

	return epoch
}

func (c *consensusRuntime) IsBridgeEnabled() bool {
	return c.config.PolyBFTConfig.IsBridgeEnabled()
}

// AddLog is an implementation of eventSubscription interface,
// and is called from the event tracker when an event is final on the rootchain
func (c *consensusRuntime) AddLog(eventLog *ethgo.Log) {
	c.logger.Info(
		"Add State sync event",
		"block", eventLog.BlockNumber,
		"hash", eventLog.TransactionHash,
		"index", eventLog.LogIndex,
	)

	event, err := decodeEvent(eventLog)
	if err != nil {
		c.logger.Error("failed to decode state sync event", "hash", eventLog.TransactionHash, "err", err)

		return
	}

	if err := c.state.insertStateSyncEvent(event); err != nil {
		c.logger.Error("failed to insert state sync event", "hash", eventLog.TransactionHash, "err", err)

		return
	}

	// update metrics
	// totalStateSyncsMeter.Mark(1)
}

// NotifyProposalInserted is an implementation of fsmNotify interface
func (c *consensusRuntime) NotifyProposalInserted(b *StateBlock) {
	lastHeader := b.Block.Header
	if c.isEndOfEpoch(lastHeader.Number) {
		// reset the epoch. Internally it updates the parent block header.
		if err := c.restartEpoch(lastHeader); err != nil {
			c.logger.Error("failed to restart epoch after block inserted", "err", err)
		}
	} else {
		// inside the epoch, update last built block header
		c.lastBuiltBlock = lastHeader
	}

	// TO DO Nemanja - probably no need for this
	// c.config.blockchain.OnNewBlockInserted(b.Block)
}

// FSM creates a new instance of fsm
func (c *consensusRuntime) FSM() (*fsm, error) {
	// figure out the parent. At this point this peer has done its best to sync up
	// to the head of their remote peers.
	parent := c.lastBuiltBlock
	epoch := c.getEpoch()

	if !epoch.Validators.ContainsNodeID(c.config.Key.NodeID()) {
		return nil, errNotAValidator
	}

	blockBuilder, err := c.config.blockchain.NewBlockBuilder(parent, types.Address(c.config.Key.Address()))
	if err != nil {
		return nil, err
	}

	pendingBlockNumber := c.getPendingBlockNumber()
	isEndOfSprint := c.isEndOfSprint(pendingBlockNumber)
	isEndOfEpoch := c.isEndOfEpoch(pendingBlockNumber)

	ff := &fsm{
		config:         c.config.PolyBFTConfig,
		parent:         parent,
		backend:        c.config.blockchain,
		polybftBackend: c.config.polybftBackend,
		blockBuilder:   blockBuilder,
		validators:     newValidatorSet(types.BytesToAddress(parent.Miner), epoch.Validators), // TO DO Nemanja - check this
		isEndOfEpoch:   isEndOfEpoch,
		isEndOfSprint:  isEndOfSprint,
		epoch:          epoch.Number,
		logger:         c.logger.Named("fsm"),
	}

	var systemState SystemState

	var nextStateSyncExecutionIdx uint64

	if c.IsBridgeEnabled() {
		systemState, err = c.getSystemState(c.lastBuiltBlock)
		if err != nil {
			return nil, err
		}

		nextStateSyncExecutionIdx, err = systemState.GetNextExecutionIndex()
		if err != nil {
			return nil, err
		}

		ff.stateSyncExecutionIndex = nextStateSyncExecutionIdx
	}

	ff.postInsertHook = func() error {
		if c.IsBridgeEnabled() {
			if isEndOfEpoch && ff.commitmentToSaveOnRegister != nil {
				if err := c.state.insertCommitmentMessage(ff.commitmentToSaveOnRegister); err != nil {
					return err
				}

				if err := c.buildBundles(
					epoch, ff.commitmentToSaveOnRegister.Message, nextStateSyncExecutionIdx); err != nil {
					return err
				}
			}
		}

		c.NotifyProposalInserted(ff.block)

		return nil
	}

	if c.IsBridgeEnabled() {
		nextRegisteredCommitmentIndex, err := systemState.GetNextCommittedIndex()
		if err != nil {
			return nil, err
		}

		if isEndOfEpoch {
			commitment, err := c.getCommitmentToRegister(epoch, nextRegisteredCommitmentIndex)
			if err != nil {
				if errors.Is(err, ErrCommitmentNotBuilt) {
					c.logger.Info("[FSM] Have no built commitment to register",
						"epoch", epoch.Number, "from state sync index", nextRegisteredCommitmentIndex)
				} else if errors.Is(err, errQuorumNotReached) {
					c.logger.Info("[FSM] Not enough votes to register commitment",
						"epoch", epoch.Number, "from state sync index", nextRegisteredCommitmentIndex)
				} else {
					return nil, err
				}
			}

			ff.proposerCommitmentToRegister = commitment
		}

		if isEndOfSprint {
			if err != nil {
				return nil, err
			}

			if err := c.state.cleanCommitments(nextStateSyncExecutionIdx); err != nil {
				return nil, err
			}

			nonExecutedCommitments, err := c.state.getNonExecutedCommitments(nextStateSyncExecutionIdx)
			if err != nil {
				return nil, err
			}

			if len(nonExecutedCommitments) > 0 {
				bundlesToExecute, err := c.state.getBundles(nextStateSyncExecutionIdx, maxBundlesPerSprint)
				if err != nil {
					return nil, err
				}

				ff.commitmentsToVerifyBundles = nonExecutedCommitments
				ff.bundleProofs = bundlesToExecute
			}
		}
	}

	if isEndOfEpoch {
		uptimeCounter, err := c.calculateUptime(parent)
		if err != nil {
			return nil, err
		}

		ff.uptimeCounter = uptimeCounter
	}

	c.logger.Info("[FSM built]",
		"epoch", epoch.Number,
		"endOfEpoch", isEndOfEpoch,
		"endOfSprint", isEndOfSprint,
	)

	return ff, nil
}

// restartEpoch resets the previously run epoch and moves to the next one
func (c *consensusRuntime) restartEpoch(header *types.Header) error {
	c.lastBuiltBlock = header

	systemState, err := c.getSystemState(c.lastBuiltBlock)
	if err != nil {
		return err
	}

	epochNumber, err := systemState.GetEpoch()
	if err != nil {
		return err
	}

	lastEpoch := c.getEpoch()
	if lastEpoch != nil {
		// Epoch might be already in memory, if its the same number do nothing.
		// Otherwise, reset the epoch metadata and restart the async services
		if lastEpoch.Number == epochNumber {
			return nil
		}
	}

	/*
		// We will uncomment this once we have the clear PoC for the checkpoint
		lastCheckpoint := uint64(0)

		// get the blocks that should be signed for this checkpoint period
		blocks := []*types.Block{}

		epochSize := c.config.Config.PolyBFT.Epoch
		for i := lastCheckpoint * epochSize; i < epoch*epochSize; i++ {
			block := c.config.Blockchain.GetBlockByNumber(i)
			if block == nil {
				panic("block not found")
			} else {
				blocks = append(blocks, block)
			}
		}
	*/

	validatorSet, err := c.config.polybftBackend.GetValidators(c.lastBuiltBlock.Number, nil)
	if err != nil {
		return err
	}

	epoch := &epochMetadata{
		Number:         epochNumber,
		LastCheckpoint: 0,
		Blocks:         []*types.Block{},
		Validators:     validatorSet,
	}

	if err := c.state.cleanEpochsFromDb(); err != nil {
		c.logger.Error("Could not clean previous epochs from db.", "err", err)
	}

	if err := c.state.insertEpoch(epoch.Number); err != nil {
		return fmt.Errorf("an error occurred while inserting new epoch in db. Reason: %w", err)
	}

	// create commitment for state sync events
	if c.IsBridgeEnabled() {
		nextCommittedIndex, err := systemState.GetNextCommittedIndex()
		if err != nil {
			return err
		}

		commitment, err := c.buildCommitment(epochNumber, nextCommittedIndex)
		if err != nil {
			return err
		}

		epoch.Commitment = commitment
	}

	c.lock.Lock()
	c.epoch = epoch
	c.lock.Unlock()

	err = c.runCheckpoint(epoch)
	if err != nil {
		return fmt.Errorf("could not run checkpoint:%w", err)
	}

	return nil
}

// buildCommitment builds a commitment message (if it is not already built in previous epoch)
// for state sync events starting from given index and saves the message in database
func (c *consensusRuntime) buildCommitment(epoch, fromIndex uint64) (*Commitment, error) {
	// if it is not already built in the previous epoch
	stateSyncEvents, err := c.state.getStateSyncEventsForCommitment(fromIndex, fromIndex+stateSyncMainBundleSize)
	if err != nil {
		if errors.Is(err, ErrNotEnoughStateSyncs) {
			c.logger.Info("[buildCommitment] Not enough state syncs to build a commitment",
				"epoch", epoch, "from state sync index", fromIndex)
			// this is a valid case, there is not enough state syncs
			return nil, nil
		}

		return nil, err
	}

	commitment, err := NewCommitment(epoch, stateSyncBundleSize, stateSyncEvents)
	if err != nil {
		return nil, err
	}

	hash := commitment.Hash().Bytes()

	signature, err := c.config.Key.Sign(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to sign commitment message. Error: %w", err)
	}

	sig := &MessageSignature{
		From:      c.config.Key.NodeID(),
		Signature: signature,
	}

	if _, err = c.state.insertMessageVote(epoch, hash, sig); err != nil {
		return nil, fmt.Errorf("failed to insert signature for hash=%v to the state."+
			"Error: %v", hex.EncodeToString(hash), err)
	}

	// gossip message
	msg := &TransportMessage{
		Hash:        hash,
		Signature:   signature,
		NodeID:      c.config.Key.NodeID(),
		EpochNumber: epoch,
	}
	c.config.Transport.Gossip(msg)

	return commitment, nil
}

// buildBundles builds bundles if there is a created commitment by the validator and inserts them into db
func (c *consensusRuntime) buildBundles(epoch *epochMetadata, commitmentMsg *CommitmentMessage,
	stateSyncExecutionIndex uint64) error {
	if epoch.Commitment == nil {
		// its a valid case when we do not have a built commitment so we can not build any proofs
		// we will be able to validate them though, since we have CommitmentMessageSigned taken from
		// register commitment state transaction when its block was inserted
		c.logger.Info("[buildProofs] No commitment built.")

		return nil
	}

	bundleProofs := []*BundleProof{}

	// TO DO Nemanja - fix this with new merkle trie

	// startBundleIdx := commitmentMsg.GetBundleIdxFromStateSyncEventIdx(stateSyncExecutionIndex)
	// for idx := startBundleIdx; idx < commitmentMsg.BundlesCount(); idx++ {
	// 	p, err := epoch.Commitment.MerkleTrie.GenerateProof(uint(idx))
	// 	if err != nil {
	// 		return err
	// 	}

	// 	events, err := c.getStateSyncEventsForBundle(commitmentMsg.GetFirstStateSyncIndexFromBundleIndex(idx),
	// 		commitmentMsg.BundleSize)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	bundleProofs = append(bundleProofs,
	// 		&BundleProof{
	// 			Proof:      p,
	// 			StateSyncs: events,
	// 		})
	// }

	return c.state.insertBundles(bundleProofs)
}

// getAggSignatureForCommitmentMessage creates aggregated signatures for given commitment
// if it has a quorum of votes
func (c *consensusRuntime) getAggSignatureForCommitmentMessage(epoch *epochMetadata,
	commitmentHash types.Hash) (Signature, error) {
	validators := epoch.Validators

	nodeIDIndexMap := make(map[pbft.NodeID]int, validators.Len())
	for i, validator := range validators {
		nodeIDIndexMap[pbft.NodeID(validator.Address.String())] = i
	}

	// get all the votes from the database for this commitment
	votes, err := c.state.getMessageVotes(epoch.Number, commitmentHash.Bytes())
	if err != nil {
		return Signature{}, err
	}

	var signatures bls.Signatures

	bitmap := bitmap.Bitmap{}

	for _, vote := range votes {
		index, exists := nodeIDIndexMap[vote.From]
		if !exists {
			continue // don't count this vote, because it does not belong to validator
		}

		signature, err := bls.UnmarshalSignature(vote.Signature)
		if err != nil {
			return Signature{}, err
		}

		bitmap.Set(uint64(index))

		signatures = append(signatures, signature)
	}

	if len(signatures) < getQuorumSize(validators.Len()) {
		return Signature{}, errQuorumNotReached
	}

	aggregatedSignature, err := signatures.Aggregate().Marshal()
	if err != nil {
		return Signature{}, err
	}

	result := Signature{
		AggregatedSignature: aggregatedSignature,
		Bitmap:              bitmap,
	}

	return result, nil
}

// getStateSyncEventsForBundle gets state sync events from database for the appropriate bundle
func (c *consensusRuntime) getStateSyncEventsForBundle(from, bundleSize uint64) ([]*StateSyncEvent, error) {
	until := bundleSize + from

	return c.state.getStateSyncEventsForCommitment(from, until)
}

// startEventTracker starts the event tracker that listens to state sync events
func (c *consensusRuntime) startEventTracker() error {
	if c.eventTracker != nil {
		return nil
	}

	c.eventTracker = &eventTracker{
		config:     c.config.PolyBFTConfig,
		subscriber: c,
		dataDir:    c.config.DataDir,
		logger:     c.logger.Named("event_tracker"),
	}

	if err := c.eventTracker.start(); err != nil {
		return err
	}

	return nil
}

// deliverMessage receives the message vote from transport and inserts it in state db for given epoch.
// It returns indicator whether message is processed successfully and error object if any.
func (c *consensusRuntime) deliverMessage(msg *TransportMessage) (bool, error) {
	epoch := c.getEpoch()
	if epoch == nil || msg.EpochNumber < epoch.Number {
		// Epoch metadata is undefined
		// or received message for some of the older epochs.
		return false, nil
	}

	if !c.isActiveValidator() {
		return false, fmt.Errorf("validator is not among the active validator set")
	}

	// check just in case
	if epoch.Validators == nil {
		return false, fmt.Errorf("validators are not set for the current epoch")
	}

	msgVote := &MessageSignature{
		From:      msg.NodeID,
		Signature: msg.Signature,
	}

	if err := validateVote(msgVote, epoch); err != nil {
		return false, err
	}

	numSignatures, err := c.state.insertMessageVote(msg.EpochNumber, msg.Hash, msgVote)
	if err != nil {
		return false, fmt.Errorf("error inserting message vote: %w", err)
	}

	c.logger.Info(
		"deliver message",
		"hash", hex.EncodeToString(msg.Hash),
		"sender", msg.NodeID,
		"signatures", numSignatures,
		"quorum", getQuorumSize(len(epoch.Validators)),
	)

	return true, nil
}

func (c *consensusRuntime) runCheckpoint(epoch *epochMetadata) error {
	// TODO: Implement checkpoint
	return nil
}

// getLatestSprintBlockNumber returns latest sprint block number
func (c *consensusRuntime) getLatestSprintBlockNumber() uint64 {
	lastBuiltBlockNumber := c.lastBuiltBlock.Number

	sprintSizeMod := lastBuiltBlockNumber % c.config.PolyBFTConfig.SprintSize
	if sprintSizeMod == 0 {
		return lastBuiltBlockNumber
	}

	sprintBlockNumber := lastBuiltBlockNumber - sprintSizeMod

	return sprintBlockNumber
}

// calculateUptime calculates uptime for blocks starting from the last built block in current epoch,
// and ending at the last block of previous epoch
func (c *consensusRuntime) calculateUptime(currentBlock *types.Header) (*UptimeCounter, error) {
	epoch := c.getEpoch()
	uptimeCounter := &UptimeCounter{validatorIndices: make(map[ethgo.Address]int)}

	if c.config.PolyBFTConfig.EpochSize < (uptimeLookbackSize + 1) {
		// this means that epoch size must at least be 3 blocks,
		// since we are not calculating uptime for lastBlockInEpoch and lastBlockInEpoch-1
		// they will be included in the uptime calculation of next epoch
		return nil, errors.New("epoch size not large enough to calculate uptime")
	}

	calculateUptimeForBlock := func(header *types.Header, validators AccountSet) error {
		blockExtra, err := GetIbftExtra(header.ExtraData)
		if err != nil {
			return err
		}

		signers, err := validators.GetFilteredValidators(blockExtra.Parent.Bitmap)
		if err != nil {
			return err
		}

		for _, a := range signers.GetAddresses() {
			uptimeCounter.AddUptime(ethgo.Address(a))
		}

		return nil
	}

	firstBlockInEpoch := calculateFirstBlockOfPeriod(currentBlock.Number, c.config.PolyBFTConfig.EpochSize)
	lastBlockInPreviousEpoch := firstBlockInEpoch - 1

	blockHeader := currentBlock
	validators := epoch.Validators

	for blockHeader.Number > firstBlockInEpoch {
		if err := calculateUptimeForBlock(blockHeader, validators); err != nil {
			return nil, err
		}

		// blockHeader, ok := c.config.blockchain.GetHeaderByNumber(blockHeader.Number - 1)
		_, _ = c.config.blockchain.GetHeaderByNumber(blockHeader.Number - 1)
	}

	// since we need to calculate uptime for the last block of the previous epoch,
	// we need to get the validators for the that epoch from the smart contract
	// this is something that should probably be optimized
	if lastBlockInPreviousEpoch > 0 { // do not calculate anything for genesis block
		for i := 0; i < uptimeLookbackSize; i++ {
			validators, err := c.config.polybftBackend.GetValidators(blockHeader.Number-2, nil)
			if err != nil {
				return nil, err
			}

			if err := calculateUptimeForBlock(blockHeader, validators); err != nil {
				return nil, err
			}

			// blockHeader, ok := c.config.blockchain.GetHeaderByNumber(blockHeader.Number - 1)
			_, _ = c.config.blockchain.GetHeaderByNumber(blockHeader.Number - 1)
		}
	}

	return uptimeCounter, nil
}

// setIsActiveValidator updates the activeValidatorFlag field
func (c *consensusRuntime) setIsActiveValidator(isActiveValidator bool) {
	if isActiveValidator {
		atomic.StoreUint32(&c.activeValidatorFlag, 1)
	} else {
		atomic.StoreUint32(&c.activeValidatorFlag, 0)
	}
}

// isActiveValidator indicates if node is in validator set or not
func (c *consensusRuntime) isActiveValidator() bool {
	return atomic.LoadUint32(&c.activeValidatorFlag) == 1
}

// getPendingBlockNumber returns block number currently being built (last built block number + 1)
func (c *consensusRuntime) getPendingBlockNumber() uint64 {
	return c.lastBuiltBlock.Number + 1
}

// isEndOfEpoch checks if an end of an epoch is reached with the current block
func (c *consensusRuntime) isEndOfEpoch(blockNumber uint64) bool {
	return isEndOfPeriod(blockNumber, c.config.PolyBFTConfig.EpochSize)
}

// isEndOfSprint checks if an end of an sprint is reached with the current block
func (c *consensusRuntime) isEndOfSprint(blockNumber uint64) bool {
	return isEndOfPeriod(blockNumber, c.config.PolyBFTConfig.SprintSize)
}

// getSystemState builds SystemState instance for the most current block header
func (c *consensusRuntime) getSystemState(header *types.Header) (SystemState, error) {
	provider, err := c.config.blockchain.GetStateProviderForBlock(header)
	if err != nil {
		return nil, err
	}

	return c.config.blockchain.GetSystemState(c.config.PolyBFTConfig, provider), nil
}

// isEndOfPeriod checks if an end of a period (either it be sprint or epoch)
// is reached with the current block (the parent block of the current fsm iteration)
func isEndOfPeriod(blockNumber, periodSize uint64) bool {
	return blockNumber%periodSize == 0
}

// getQuorumSize returns result of division of given number by two,
// but rounded to next integer value (similar to math.Ceil function).
func getQuorumSize(validatorsCount int) int {
	return (validatorsCount + 1) / 2
}

// calculateFirstBlockOfPeriod calculates the first block of a period
func calculateFirstBlockOfPeriod(currentBlockNumber, periodSize uint64) uint64 {
	if currentBlockNumber <= periodSize {
		return 1 // it's the first epoch
	}

	switch currentBlockNumber % periodSize {
	case 1:
		return currentBlockNumber
	case 0:
		return currentBlockNumber - periodSize + 1
	default:
		return currentBlockNumber - (currentBlockNumber % periodSize) + 1
	}
}

// getEpochNumber returns epoch number for given blockNumber and epochSize.
// Epoch number is derived as a result of division of block number and epoch size.
// Since epoch number is 1-based (0 block represents special case zero epoch),
// we are incrementing result by one for non epoch-ending blocks.
func getEpochNumber(blockNumber, epochSize uint64) uint64 {
	if isEndOfPeriod(blockNumber, epochSize) {
		return blockNumber / epochSize
	}

	return blockNumber/epochSize + 1
}

// getEndEpochBlockNumber returns block number which corresponds
// to the one at the beginning of the given epoch with regards to epochSize
func getEndEpochBlockNumber(epoch, epochSize uint64) uint64 {
	return epoch * epochSize
}

// getCommitmentToRegister gets commitments to register via state transaction
func (c *consensusRuntime) getCommitmentToRegister(epoch *epochMetadata,
	registerCommitmentIndex uint64) (*CommitmentMessageSigned, error) {
	if epoch.Commitment == nil {
		// we did not build a commitment, so there is nothing to register
		return nil, ErrCommitmentNotBuilt
	}

	commitmentMessage := NewCommitmentMessage(
		// epoch.Commitment.MerkleTrie.Trie.Hash(),
		types.EmptyRootHash, // TO DO Nemanja - fix this with bridge
		registerCommitmentIndex,
		registerCommitmentIndex+stateSyncMainBundleSize-1,
		epoch.Number,
		stateSyncBundleSize)

	aggregatedSignature, err := c.getAggSignatureForCommitmentMessage(epoch, epoch.Commitment.Hash())
	if err != nil {
		return nil, err
	}

	return &CommitmentMessageSigned{
		Message:      commitmentMessage,
		AggSignature: aggregatedSignature,
	}, nil
}

// createStateTransaction creates a state transaction out of provided parameters.
// args parameter is ABI encoded against provided ABI method.
func createStateTransaction(
	target types.Address,
	abiMethod *abi.Method,
	args interface{},
) (*types.Transaction, error) {
	abiEncodedArgs, err := abiMethod.Encode(args)
	if err != nil {
		return nil, fmt.Errorf("failed to encode arguments:%w", err)
	}

	return createStateTransactionWithData(target, abiEncodedArgs, stateTransactionsGasLimit), nil
}

// createStateTransactionWithData creates a state transaction
// with provided target address and inputData parameter which is ABI encoded byte array.
func createStateTransactionWithData(target types.Address, inputData []byte, gasLimit uint64) *types.Transaction {
	// return types.NewTx(
	// 	&types.StateTransaction{
	// 		To:    target,
	// 		Input: inputData,
	// 		Gas:   gasLimit,
	// 	})
	// TO DO Nemanja - fix this with bridge
	return nil
}

func validateVote(vote *MessageSignature, epoch *epochMetadata) error {
	// get senders address
	senderAddress := types.StringToAddress(string(vote.From))
	if !epoch.Validators.ContainsAddress(senderAddress) {
		return fmt.Errorf(
			"message is received from sender %s, which is not in current validator set",
			vote.From,
		)
	}

	return nil
}