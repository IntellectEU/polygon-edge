package polybft

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path"
	"sync"

	"github.com/0xPolygon/polygon-edge/consensus/polybft/bitmap"
	polybftProto "github.com/0xPolygon/polygon-edge/consensus/polybft/proto"
	bls "github.com/0xPolygon/polygon-edge/consensus/polybft/signer"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/wallet"
	"github.com/0xPolygon/polygon-edge/tracker"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/umbracle/ethgo"
	"google.golang.org/protobuf/proto"
)

const (
	// minimum number of stateSyncEvents that a commitment can have
	// (minimum number is 2 because smart contract expects that the merkle tree has at least two leaves)
	minCommitmentSize = 2
)

// StateSyncManager is an interface that defines functions for state sync workflow
type StateSyncManager interface {
	Init() error
	Close()
	Commitment() (*CommitmentMessageSigned, error)
	GetStateSyncProof(stateSyncID uint64) (*types.StateSyncProof, error)
	PostBlock(req *PostBlockRequest) error
	PostEpoch(req *PostEpochRequest) error
}

var _ StateSyncManager = (*dummyStateSyncManager)(nil)

// dummyStateSyncManager is used when bridge is not enabled
type dummyStateSyncManager struct{}

func (n *dummyStateSyncManager) Init() error                                   { return nil }
func (n *dummyStateSyncManager) Close()                                        {}
func (n *dummyStateSyncManager) Commitment() (*CommitmentMessageSigned, error) { return nil, nil }
func (n *dummyStateSyncManager) PostBlock(req *PostBlockRequest) error         { return nil }
func (n *dummyStateSyncManager) PostEpoch(req *PostEpochRequest) error         { return nil }
func (n *dummyStateSyncManager) GetStateSyncProof(stateSyncID uint64) (*types.StateSyncProof, error) {
	return nil, nil
}

// stateSyncConfig holds the configuration data of state sync manager
type stateSyncConfig struct {
	stateSenderAddr   types.Address
	jsonrpcAddr       string
	dataDir           string
	topic             topic
	key               *wallet.Key
	maxCommitmentSize uint64
}

var _ StateSyncManager = (*stateSyncManager)(nil)

// stateSyncManager is a struct that manages the workflow of
// saving and querying state sync events, and creating, and submitting new commitments
type stateSyncManager struct {
	logger hclog.Logger
	state  *State

	config  *stateSyncConfig
	closeCh chan struct{}

	// per epoch fields
	lock               sync.RWMutex
	pendingCommitments []*Commitment
	validatorSet       ValidatorSet
	epoch              uint64
	nextCommittedIndex uint64
}

// topic is an interface for p2p message gossiping
type topic interface {
	Publish(obj proto.Message) error
	Subscribe(handler func(obj interface{}, from peer.ID)) error
}

// NewStateSyncManager creates a new instance of state sync manager
func NewStateSyncManager(logger hclog.Logger, state *State, config *stateSyncConfig) (*stateSyncManager, error) {
	s := &stateSyncManager{
		logger:  logger.Named("state-sync-manager"),
		state:   state,
		config:  config,
		closeCh: make(chan struct{}),
	}

	return s, nil
}

// Init subscribes to bridge topics (getting votes) and start the event tracker routine
func (s *stateSyncManager) Init() error {
	if err := s.initTracker(); err != nil {
		return fmt.Errorf("failed to init event tracker. Error: %w", err)
	}

	if err := s.initTransport(); err != nil {
		return fmt.Errorf("failed to initialize state sync transport layer. Error: %w", err)
	}

	return nil
}

func (s *stateSyncManager) Close() {
	close(s.closeCh)
}

// initTracker starts a new event tracker (to receive new state sync events)
func (s *stateSyncManager) initTracker() error {
	ctx, cancelFn := context.WithCancel(context.Background())

	tracker := tracker.NewEventTracker(
		path.Join(s.config.dataDir, "/deposit.db"),
		s.config.jsonrpcAddr,
		ethgo.Address(s.config.stateSenderAddr),
		s,
		s.logger)

	go func() {
		<-s.closeCh
		cancelFn()
	}()

	return tracker.Start(ctx)
}

// initTransport subscribes to bridge topics (getting votes for commitments)
func (s *stateSyncManager) initTransport() error {
	return s.config.topic.Subscribe(func(obj interface{}, _ peer.ID) {
		msg, ok := obj.(*polybftProto.TransportMessage)
		if !ok {
			s.logger.Warn("failed to deliver vote, invalid msg", "obj", obj)

			return
		}

		var transportMsg *TransportMessage

		if err := json.Unmarshal(msg.Data, &transportMsg); err != nil {
			s.logger.Warn("failed to deliver vote", "error", err)

			return
		}

		if err := s.saveVote(transportMsg); err != nil {
			s.logger.Warn("failed to deliver vote", "error", err)
		}
	})
}

// saveVote saves the gotten vote to boltDb for later quorum check and signature aggregation
func (s *stateSyncManager) saveVote(msg *TransportMessage) error {
	s.lock.RLock()
	epoch := s.epoch
	valSet := s.validatorSet
	s.lock.RUnlock()

	if valSet == nil || msg.EpochNumber < epoch {
		// Epoch metadata is undefined or received message for some of the older epochs
		return nil
	}

	if !valSet.Includes(types.StringToAddress(msg.NodeID)) {
		return fmt.Errorf("validator is not among the active validator set")
	}

	msgVote := &MessageSignature{
		From:      msg.NodeID,
		Signature: msg.Signature,
	}

	numSignatures, err := s.state.insertMessageVote(msg.EpochNumber, msg.Hash, msgVote)
	if err != nil {
		return fmt.Errorf("error inserting message vote: %w", err)
	}

	s.logger.Info(
		"deliver message",
		"hash", hex.EncodeToString(msg.Hash),
		"sender", msg.NodeID,
		"signatures", numSignatures,
	)

	return nil
}

// AddLog saves the received log from event tracker if it matches a state sync event ABI
func (s *stateSyncManager) AddLog(eventLog *ethgo.Log) {
	if !stateTransferEventABI.Match(eventLog) {
		return
	}

	s.logger.Info(
		"Add State sync event",
		"block", eventLog.BlockNumber,
		"hash", eventLog.TransactionHash,
		"index", eventLog.LogIndex,
	)

	event, err := decodeStateSyncEvent(eventLog)
	if err != nil {
		s.logger.Error("could not decode state sync event", "err", err)

		return
	}

	if err := s.state.insertStateSyncEvent(event); err != nil {
		s.logger.Error("could not save state sync event to boltDb", "err", err)

		return
	}

	if err := s.buildCommitment(); err != nil {
		s.logger.Error("could not build a commitment on arrival of new state sync", "err", err, "stateSyncID", event.ID)
	}
}

// Commitment returns a commitment to be submitted if there is a pending commitment with quorum
func (s *stateSyncManager) Commitment() (*CommitmentMessageSigned, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	var largestCommitment *CommitmentMessageSigned

	// we start from the end, since last pending commitment is the largest one
	for i := len(s.pendingCommitments) - 1; i >= 0; i-- {
		commitment := s.pendingCommitments[i]
		aggregatedSignature, publicKeys, err := s.getAggSignatureForCommitmentMessage(commitment)

		if err != nil {
			if errors.Is(err, errQuorumNotReached) {
				// a valid case, commitment has no quorum, we should not return an error
				s.logger.Debug("can not submit a commitment, quorum not reached",
					"from", commitment.FromIndex,
					"to", commitment.ToIndex)

				continue
			}

			return nil, err
		}

		largestCommitment = &CommitmentMessageSigned{
			Message: NewCommitmentMessage(
				commitment.MerkleTree.Hash(),
				commitment.FromIndex,
				commitment.ToIndex),
			AggSignature: aggregatedSignature,
			PublicKeys:   publicKeys,
		}

		break
	}

	return largestCommitment, nil
}

// getAggSignatureForCommitmentMessage checks if pending commitment has quorum,
// and if it does, aggregates the signatures
func (s *stateSyncManager) getAggSignatureForCommitmentMessage(commitment *Commitment) (Signature, [][]byte, error) {
	validatorSet := s.validatorSet

	validatorAddrToIndex := make(map[string]int, validatorSet.Len())
	validatorsMetadata := validatorSet.Accounts()

	for i, validator := range validatorsMetadata {
		validatorAddrToIndex[validator.Address.String()] = i
	}

	commitmentHash, err := commitment.Hash()
	if err != nil {
		return Signature{}, nil, err
	}

	// get all the votes from the database for this commitment
	votes, err := s.state.getMessageVotes(commitment.Epoch, commitmentHash.Bytes())
	if err != nil {
		return Signature{}, nil, err
	}

	var signatures bls.Signatures

	publicKeys := make([][]byte, 0)
	bitmap := bitmap.Bitmap{}
	signers := make(map[types.Address]struct{}, 0)

	for _, vote := range votes {
		index, exists := validatorAddrToIndex[vote.From]
		if !exists {
			continue // don't count this vote, because it does not belong to validator
		}

		signature, err := bls.UnmarshalSignature(vote.Signature)
		if err != nil {
			return Signature{}, nil, err
		}

		bitmap.Set(uint64(index))

		signatures = append(signatures, signature)
		publicKeys = append(publicKeys, validatorsMetadata[index].BlsKey.Marshal())
		signers[types.StringToAddress(vote.From)] = struct{}{}
	}

	if !validatorSet.HasQuorum(signers) {
		return Signature{}, nil, errQuorumNotReached
	}

	aggregatedSignature, err := signatures.Aggregate().Marshal()
	if err != nil {
		return Signature{}, nil, err
	}

	result := Signature{
		AggregatedSignature: aggregatedSignature,
		Bitmap:              bitmap,
	}

	return result, publicKeys, nil
}

// PostEpoch notifies the state sync manager that an epoch has changed,
// so that it can discard any previous epoch commitments, and build a new one (since validator set changed)
func (s *stateSyncManager) PostEpoch(req *PostEpochRequest) error {
	s.lock.Lock()

	s.pendingCommitments = nil
	s.validatorSet = req.ValidatorSet
	s.epoch = req.NewEpochID

	// build a new commitment at the end of the epoch
	nextCommittedIndex, err := req.SystemState.GetNextCommittedIndex()
	if err != nil {
		s.lock.Unlock()

		return err
	}

	s.nextCommittedIndex = nextCommittedIndex

	s.lock.Unlock()

	return s.buildCommitment()
}

// PostBlock notifies state sync manager that a block was finalized,
// so that it can build state sync proofs if a block has a commitment submission transaction
func (s *stateSyncManager) PostBlock(req *PostBlockRequest) error {
	commitment, err := getCommitmentMessageSignedTx(req.FullBlock.Block.Transactions)
	if err != nil {
		return err
	}

	// no commitment message -> this is not end of epoch block
	if commitment == nil {
		return nil
	}

	if err := s.state.insertCommitmentMessage(commitment); err != nil {
		return fmt.Errorf("insert commitment message error: %w", err)
	}

	if err := s.buildProofs(commitment.Message); err != nil {
		return fmt.Errorf("build commitment proofs error: %w", err)
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	// update the nextCommittedIndex since a commitment was submitted
	s.nextCommittedIndex = commitment.Message.ToIndex + 1
	// commitment was submitted, so discard what we have in memory, so we can build a new one
	s.pendingCommitments = nil

	return nil
}

// GetStateSyncProof returns the proof for the state sync
func (s *stateSyncManager) GetStateSyncProof(stateSyncID uint64) (*types.StateSyncProof, error) {
	proof, err := s.state.getStateSyncProof(stateSyncID)
	if err != nil {
		return nil, fmt.Errorf("cannot get state sync proof for StateSync id %d: %w", stateSyncID, err)
	}

	if proof == nil {
		// check if we might've missed a commitment. if it is so, we didn't build proofs for it while syncing
		// if we are all synced up, commitment will be saved through PostBlock, but we wont have proofs,
		// so we will build them now and save them to db so that we have proofs for missed commitment
		commitment, err := s.state.getCommitmentForStateSync(stateSyncID)
		if err != nil {
			return nil, fmt.Errorf("cannot find commitment for StateSync id %d: %w", stateSyncID, err)
		}

		if err := s.buildProofs(commitment.Message); err != nil {
			return nil, fmt.Errorf("cannot build proofs for commitment for StateSync id %d: %w", stateSyncID, err)
		}

		proof, err = s.state.getStateSyncProof(stateSyncID)
		if err != nil {
			return nil, fmt.Errorf("cannot get state sync proof for StateSync id %d: %w", stateSyncID, err)
		}
	}

	return proof, nil
}

// buildProofs builds state sync proofs for the submitted commitment and saves them in boltDb for later execution
func (s *stateSyncManager) buildProofs(commitmentMsg *CommitmentMessage) error {
	s.logger.Debug(
		"[buildProofs] Building proofs for commitment...",
		"fromIndex", commitmentMsg.FromIndex,
		"toIndex", commitmentMsg.ToIndex,
	)

	events, err := s.state.getStateSyncEventsForCommitment(commitmentMsg.FromIndex, commitmentMsg.ToIndex)
	if err != nil {
		return fmt.Errorf("failed to get state sync events for commitment to build proofs. Error: %w", err)
	}

	tree, err := createMerkleTree(events)
	if err != nil {
		return err
	}

	stateSyncProofs := make([]*types.StateSyncProof, len(events))

	for i, event := range events {
		p := tree.GenerateProof(uint64(i), 0)

		stateSyncProofs[i] = &types.StateSyncProof{
			Proof:     p,
			StateSync: event,
		}
	}

	s.logger.Debug(
		"[buildProofs] Building proofs for commitment finished.",
		"fromIndex", commitmentMsg.FromIndex,
		"toIndex", commitmentMsg.ToIndex,
	)

	return s.state.insertStateSyncProofs(stateSyncProofs)
}

// buildCommitment builds a new commitment, signs it and gossips its vote for it
func (s *stateSyncManager) buildCommitment() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	epoch := s.epoch
	fromIndex := s.nextCommittedIndex

	stateSyncEvents, err := s.state.getStateSyncEventsForCommitment(fromIndex,
		fromIndex+s.config.maxCommitmentSize-1)
	if err != nil && !errors.Is(err, errNotEnoughStateSyncs) {
		return fmt.Errorf("failed to get state sync events for commitment. Error: %w", err)
	}

	if len(stateSyncEvents) < minCommitmentSize {
		// there is not enough state sync events to build at least the minimum commitment
		return nil
	}

	if len(s.pendingCommitments) > 0 &&
		s.pendingCommitments[len(s.pendingCommitments)-1].ToIndex >= stateSyncEvents[len(stateSyncEvents)-1].ID {
		// already built a commitment of this size which is pending to be submitted
		return nil
	}

	commitment, err := NewCommitment(epoch, stateSyncEvents)
	if err != nil {
		return err
	}

	hash, err := commitment.Hash()
	if err != nil {
		return fmt.Errorf("failed to generate hash for commitment. Error: %w", err)
	}

	hashBytes := hash.Bytes()

	signature, err := s.config.key.Sign(hashBytes)
	if err != nil {
		return fmt.Errorf("failed to sign commitment message. Error: %w", err)
	}

	sig := &MessageSignature{
		From:      s.config.key.String(),
		Signature: signature,
	}

	if _, err = s.state.insertMessageVote(epoch, hashBytes, sig); err != nil {
		return fmt.Errorf(
			"failed to insert signature for hash=%v to the state. Error: %w",
			hex.EncodeToString(hashBytes),
			err,
		)
	}

	// gossip message
	s.multicast(&TransportMessage{
		Hash:        hashBytes,
		Signature:   signature,
		NodeID:      s.config.key.String(),
		EpochNumber: epoch,
	})

	s.logger.Debug(
		"[buildCommitment] Built commitment",
		"from", commitment.FromIndex,
		"to", commitment.ToIndex,
	)

	s.pendingCommitments = append(s.pendingCommitments, commitment)

	return nil
}

// multicast publishes given message to the rest of the network
func (s *stateSyncManager) multicast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		s.logger.Warn("failed to marshal bridge message", "err", err)

		return
	}

	err = s.config.topic.Publish(&polybftProto.TransportMessage{Data: data})
	if err != nil {
		s.logger.Warn("failed to gossip bridge message", "err", err)
	}
}

// newStateSyncEvent creates an instance of pending state sync event.
func newStateSyncEvent(
	id uint64,
	sender ethgo.Address,
	target ethgo.Address,
	data []byte,
) *types.StateSyncEvent {
	return &types.StateSyncEvent{
		ID:       id,
		Sender:   sender,
		Receiver: target,
		Data:     data,
	}
}

func decodeStateSyncEvent(log *ethgo.Log) (*types.StateSyncEvent, error) {
	raw, err := stateTransferEventABI.ParseLog(log)
	if err != nil {
		return nil, err
	}

	eventGeneric, err := decodeEventData(raw, log,
		func(id *big.Int, sender, receiver ethgo.Address, data []byte) interface{} {
			return newStateSyncEvent(id.Uint64(), sender, receiver, data)
		})
	if err != nil {
		return nil, err
	}

	stateSyncEvent, ok := eventGeneric.(*types.StateSyncEvent)
	if !ok {
		return nil, errors.New("failed to convert event to StateSyncEvent instance")
	}

	return stateSyncEvent, nil
}