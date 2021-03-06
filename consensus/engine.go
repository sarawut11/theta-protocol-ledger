package consensus

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/thetatoken/theta/blockchain"
	"github.com/thetatoken/theta/common"
	"github.com/thetatoken/theta/common/util"
	"github.com/thetatoken/theta/core"
	"github.com/thetatoken/theta/crypto"
	"github.com/thetatoken/theta/dispatcher"
	"github.com/thetatoken/theta/rlp"
	"github.com/thetatoken/theta/store"
)

var logger *log.Entry = log.WithFields(log.Fields{"prefix": "consensus"})

var _ core.ConsensusEngine = (*ConsensusEngine)(nil)

// ConsensusEngine is the default implementation of the Engine interface.
type ConsensusEngine struct {
	logger *log.Entry

	privateKey *crypto.PrivateKey

	chain            *blockchain.Chain
	dispatcher       *dispatcher.Dispatcher
	validatorManager core.ValidatorManager
	ledger           core.Ledger

	incoming        chan interface{}
	finalizedBlocks chan *core.Block

	// Life cycle
	wg      *sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool

	mu            *sync.Mutex
	epochTimer    *time.Timer
	proposalTimer *time.Timer

	state *State

	rand *rand.Rand
}

// NewConsensusEngine creates a instance of ConsensusEngine.
func NewConsensusEngine(privateKey *crypto.PrivateKey, db store.Store, chain *blockchain.Chain, dispatcher *dispatcher.Dispatcher, validatorManager core.ValidatorManager) *ConsensusEngine {
	e := &ConsensusEngine{
		chain:      chain,
		dispatcher: dispatcher,

		privateKey: privateKey,

		incoming:        make(chan interface{}, viper.GetInt(common.CfgConsensusMessageQueueSize)),
		finalizedBlocks: make(chan *core.Block, viper.GetInt(common.CfgConsensusMessageQueueSize)),

		wg: &sync.WaitGroup{},

		mu:    &sync.Mutex{},
		state: NewState(db, chain),

		validatorManager: validatorManager,
	}

	logger = util.GetLoggerForModule("consensus")
	e.logger = logger

	e.logger.WithFields(log.Fields{"state": e.state}).Info("Starting state")

	e.rand = rand.New(rand.NewSource(time.Now().Unix()))

	return e
}

func (e *ConsensusEngine) SetLedger(ledger core.Ledger) {
	e.ledger = ledger
}

// GetLedger returns the ledger instance attached to the consensus engine
func (e *ConsensusEngine) GetLedger() core.Ledger {
	return e.ledger
}

// ID returns the identifier of current node.
func (e *ConsensusEngine) ID() string {
	return e.privateKey.PublicKey().Address().Hex()
}

// PrivateKey returns the private key
func (e *ConsensusEngine) PrivateKey() *crypto.PrivateKey {
	return e.privateKey
}

// Chain return a pointer to the underlying chain store.
func (e *ConsensusEngine) Chain() *blockchain.Chain {
	return e.chain
}

// GetEpoch returns the current epoch
func (e *ConsensusEngine) GetEpoch() uint64 {
	return e.state.GetEpoch()
}

// GetValidatorManager returns a pointer to the valiator manager.
func (e *ConsensusEngine) GetValidatorManager() core.ValidatorManager {
	return e.validatorManager
}

// Start starts sub components and kick off the main loop.
func (e *ConsensusEngine) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	e.ctx = c
	e.cancel = cancel

	// Verify configurations
	if viper.GetInt(common.CfgConsensusMaxEpochLength) <= viper.GetInt(common.CfgConsensusMinProposalWait) {
		log.WithFields(log.Fields{
			"CfgConsensusMaxEpochLength":  viper.GetInt(common.CfgConsensusMaxEpochLength),
			"CfgConsensusMinProposalWait": viper.GetInt(common.CfgConsensusMinProposalWait),
		}).Fatal("Invalid configuration: max epoch length must be larger than minimal proposal wait")
	}

	// Set ledger state pointer to intial state.
	lastCC := e.state.GetHighestCCBlock()
	e.ledger.ResetState(lastCC.Height, lastCC.StateHash)

	e.wg.Add(1)
	go e.mainLoop()
}

// Stop notifies all goroutines to stop without blocking.
func (e *ConsensusEngine) Stop() {
	e.cancel()
}

// Wait blocks until all goroutines stop.
func (e *ConsensusEngine) Wait() {
	e.wg.Wait()
}

func (e *ConsensusEngine) mainLoop() {
	defer e.wg.Done()

	for {
		e.enterEpoch()
	Epoch:
		for {
			select {
			case <-e.ctx.Done():
				e.stopped = true
				return
			case msg := <-e.incoming:
				endEpoch := e.processMessage(msg)
				if endEpoch {
					break Epoch
				}
			case <-e.epochTimer.C:
				e.logger.WithFields(log.Fields{"e.epoch": e.GetEpoch()}).Debug("Epoch timeout. Repeating epoch")
				e.vote()
				break Epoch
			case <-e.proposalTimer.C:
				e.propose()
			}
		}
	}
}

func (e *ConsensusEngine) enterEpoch() {
	// Reset timers.
	if e.epochTimer != nil {
		e.epochTimer.Stop()
	}
	e.epochTimer = time.NewTimer(time.Duration(viper.GetInt(common.CfgConsensusMaxEpochLength)) * time.Second)

	if e.proposalTimer != nil {
		e.proposalTimer.Stop()
	}
	if e.shouldPropose(e.GetEpoch()) {
		e.proposalTimer = time.NewTimer(time.Duration(viper.GetInt(common.CfgConsensusMinProposalWait)) * time.Second)
	} else {
		e.proposalTimer = time.NewTimer(math.MaxInt64)
		e.proposalTimer.Stop()
	}
}

// GetChannelIDs implements the p2p.MessageHandler interface.
func (e *ConsensusEngine) GetChannelIDs() []common.ChannelIDEnum {
	return []common.ChannelIDEnum{
		common.ChannelIDHeader,
		common.ChannelIDBlock,
		common.ChannelIDVote,
	}
}

func (e *ConsensusEngine) AddMessage(msg interface{}) {
	e.incoming <- msg
}

func (e *ConsensusEngine) processMessage(msg interface{}) (endEpoch bool) {
	switch m := msg.(type) {
	case core.Vote:
		e.logger.WithFields(log.Fields{"vote": m}).Debug("Received vote")
		return e.handleStandaloneVote(m)
	case *core.Block:
		e.logger.WithFields(log.Fields{"block": m}).Debug("Received block")
		e.handleBlock(m)
	default:
		log.Errorf("Unknown message type: %v", m)
		panic(fmt.Sprintf("Unknown message type: %v", m))
	}

	return false
}

func (e *ConsensusEngine) validateBlock(block *core.Block, parent *core.ExtendedBlock) bool {
	validators := e.validatorManager.GetValidatorSet(block.Hash())

	if parent.Height+1 != block.Height {
		e.logger.WithFields(log.Fields{
			"parent":        block.Parent.Hex(),
			"parent.Height": parent.Height,
			"block":         block.Hash().Hex(),
			"block.Height":  block.Height,
		}).Warn("Block.Height != parent.Height + 1")
		return false
	}

	if parent.Epoch >= block.Epoch {
		e.logger.WithFields(log.Fields{
			"parent":       block.Parent.Hex(),
			"parent.Epoch": parent.Epoch,
			"block":        block.Hash().Hex(),
			"block.Epoch":  block.Epoch,
		}).Warn("Block.Epoch <= parent.Epoch")
		return false
	}

	if !parent.Status.IsValid() {
		e.logger.WithFields(log.Fields{
			"parent": block.Parent.Hex(),
			"block":  block.Hash().Hex(),
		}).Warn("Block is referring to invalid parent block")
		return false
	}

	if !e.chain.IsDescendant(block.HCC.BlockHash, block.Hash()) {
		e.logger.WithFields(log.Fields{
			"block.HCC": block.HCC.BlockHash.Hex(),
			"block":     block.Hash().Hex(),
		}).Warn("HCC must be ancestor")
		return false
	}

	if !block.HCC.IsValid(validators) {
		e.logger.WithFields(log.Fields{
			"parent":    block.Parent.Hex(),
			"block":     block.Hash().Hex(),
			"block.HCC": block.HCC.String(),
		}).Warn("Invalid HCC")
		return false
	}

	// Blocks with validator changes must be followed by two direct confirmation blocks.
	if parent.HasValidatorUpdate {
		if block.HCC.BlockHash != block.Parent {
			e.logger.WithFields(log.Fields{
				"parent":    block.Parent.Hex(),
				"block":     block.Hash().Hex(),
				"block.HCC": block.HCC.BlockHash.Hex(),
			}).Warn("block.HCC must equal to parent when parent contains validator changes.")
			return false
		}
	}
	if !parent.Parent.IsEmpty() {
		grandParent, err := e.chain.FindBlock(parent.Parent)
		if err != nil {
			e.logger.WithFields(log.Fields{
				"error":         err,
				"parent":        parent.Hash().Hex(),
				"block":         block.Hash().Hex(),
				"parent.Parent": parent.Parent.Hex(),
			}).Warn("Failed to find grand parent block")
			return false
		}
		if grandParent.HasValidatorUpdate {
			if block.HCC.BlockHash != block.Parent {
				e.logger.WithFields(log.Fields{
					"parent":    block.Parent.Hex(),
					"block":     block.Hash().Hex(),
					"block.HCC": block.HCC.BlockHash.Hex(),
				}).Warn("block.HCC must equal to block.Parent when block.Parent.Parent contains validator changes.")
				return false
			}
			if !block.HCC.IsProven(validators) {
				e.logger.WithFields(log.Fields{
					"parent":    block.Parent.Hex(),
					"block":     block.Hash().Hex(),
					"block.HCC": block.HCC,
				}).Warn("block.HCC must contain valid voteset when block.Parent.Parent contains validator changes.")
				return false
			}
		}
	}

	if res := block.Validate(); res.IsError() {
		e.logger.WithFields(log.Fields{
			"err": res.String(),
		}).Warn("Block is invalid")
		return false
	}
	if !e.shouldProposeByID(block.Epoch, block.Proposer.Hex()) {
		e.logger.WithFields(log.Fields{
			"block.Epoch":    block.Epoch,
			"block.proposer": block.Proposer.Hex(),
		}).Warn("Invalid proposer")
		return false
	}
	return true
}

func (e *ConsensusEngine) handleBlock(block *core.Block) {
	parent, err := e.chain.FindBlock(block.Parent)
	if err != nil {
		// Should not happen.
		e.logger.WithFields(log.Fields{
			"error":  err,
			"parent": block.Parent.Hex(),
			"block":  block.Hash().Hex(),
		}).Fatal("Failed to find parent block")
		return
	}

	if !e.validateBlock(block, parent) {
		e.chain.MarkBlockInvalid(block.Hash())
		e.logger.WithFields(log.Fields{
			"block.Hash": block.Hash().Hex(),
		}).Warn("Block is invalid")
		return
	}

	for _, vote := range block.HCC.Votes.Votes() {
		e.handleVoteInBlock(vote)
	}

	result := e.ledger.ResetState(parent.Height, parent.StateHash)
	if result.IsError() {
		e.logger.WithFields(log.Fields{
			"error":            result.Message,
			"parent.StateHash": parent.StateHash,
		}).Error("Failed to reset state to parent.StateHash")
		return
	}
	result = e.ledger.ApplyBlockTxs(block.Txs, block.StateHash)
	if result.IsError() {
		e.logger.WithFields(log.Fields{
			"error":           result.String(),
			"parent":          block.Parent.Hex(),
			"block":           block.Hash().Hex(),
			"block.StateHash": block.StateHash.Hex(),
		}).Error("Failed to apply block Txs")
		return
	}

	if hasValidatorUpdate, ok := result.Info["hasValidatorUpdate"]; ok {
		hasValidatorUpdateBool := hasValidatorUpdate.(bool)
		if hasValidatorUpdateBool {
			e.chain.MarkBlockHasValidatorUpdate(block.Hash())
		}
	}

	e.chain.MarkBlockValid(block.Hash())

	// Check and process CC.
	e.checkCC(block.Hash())

	// Skip voting for block older than current best known epoch.
	// Allow block with one epoch behind since votes are processed first and might advance epoch
	// before block is processed.
	if block.Epoch < e.GetEpoch()-1 {
		e.logger.WithFields(log.Fields{
			"block.Epoch": block.Epoch,
			"block.Hash":  block.Hash().Hex(),
			"e.epoch":     e.GetEpoch(),
		}).Debug("Skipping voting for block from previous epoch")
		return
	}

	e.vote()
}

func (e *ConsensusEngine) shouldVote(block common.Hash) bool {
	return e.shouldVoteByID(e.privateKey.PublicKey().Address(), block)
}

func (e *ConsensusEngine) shouldVoteByID(id common.Address, block common.Hash) bool {
	validators := e.validatorManager.GetValidatorSet(block)
	_, err := validators.GetValidator(id)
	return err == nil
}

func (e *ConsensusEngine) vote() {
	tip := e.GetTipToVote()

	if !e.shouldVote(tip.Hash()) {
		return
	}

	var vote core.Vote
	lastVote := e.state.GetLastVote()
	shouldRepeatVote := false
	if lastVote.Height != 0 && lastVote.Height >= tip.Height {
		// Voting height should be monotonically increasing.
		e.logger.WithFields(log.Fields{
			"lastVote.Height": lastVote.Height,
			"tip.Height":      tip.Height,
		}).Debug("Repeating vote at height")
		shouldRepeatVote = true
	} else if localHCC := e.state.GetHighestCCBlock().Hash(); lastVote.Height != 0 && tip.HCC.BlockHash != localHCC {
		// HCC in candidate block must equal local highest CC.
		e.logger.WithFields(log.Fields{
			"tip.HCC":   tip.HCC.BlockHash.Hex(),
			"local.HCC": localHCC.Hex(),
		}).Debug("Repeating vote due to mismatched HCC")
		shouldRepeatVote = true
	}

	if shouldRepeatVote {
		block, err := e.chain.FindBlock(lastVote.Block)
		if err != nil {
			log.Panic(err)
		}
		// Recreating vote so that it has updated epoch and signature.
		vote = e.createVote(block.Block)
	} else {
		vote = e.createVote(tip.Block)
		e.state.SetLastVote(vote)
	}
	e.logger.WithFields(log.Fields{
		"vote": vote,
	}).Debug("Sending vote")
	e.broadcastVote(vote)
	e.handleVote(vote)
}

func (e *ConsensusEngine) broadcastVote(vote core.Vote) {
	payload, err := rlp.EncodeToBytes(vote)
	if err != nil {
		e.logger.WithFields(log.Fields{"vote": vote}).Error("Failed to encode vote")
		return
	}
	voteMsg := dispatcher.DataResponse{
		ChannelID: common.ChannelIDVote,
		Payload:   payload,
	}
	e.dispatcher.SendData([]string{}, voteMsg)
}

func (e *ConsensusEngine) createVote(block *core.Block) core.Vote {
	vote := core.Vote{
		Block:  block.Hash(),
		Height: block.Height,
		ID:     e.privateKey.PublicKey().Address(),
		Epoch:  e.GetEpoch(),
	}
	sig, err := e.privateKey.Sign(vote.SignBytes())
	if err != nil {
		e.logger.WithFields(log.Fields{"error": err}).Panic("Failed to sign vote")
	}
	vote.SetSignature(sig)
	return vote
}

func (e *ConsensusEngine) validateVote(vote core.Vote) bool {
	if res := vote.Validate(); res.IsError() {
		e.logger.WithFields(log.Fields{
			"err": res.String(),
		}).Warn("Ignoring invalid vote")
		return false
	}
	return true
}

func (e *ConsensusEngine) handleVoteInBlock(vote core.Vote) (endEpoch bool) {
	return e.handleVote(vote)
}

func (e *ConsensusEngine) handleStandaloneVote(vote core.Vote) (endEpoch bool) {
	endEpoch = e.handleVote(vote)
	e.checkCC(vote.Block)
	return
}

func (e *ConsensusEngine) handleVote(vote core.Vote) (endEpoch bool) {
	// Validate vote.
	if !e.validateVote(vote) {
		return
	}

	// Save vote.
	err := e.state.AddVote(&vote)
	if err != nil {
		e.logger.WithFields(log.Fields{"err": err}).Panic("Failed to add vote")
	}

	// Update epoch.
	lfb := e.state.GetLastFinalizedBlock()
	nextValidators := e.validatorManager.GetNextValidatorSet(lfb.Hash())
	if vote.Epoch >= e.GetEpoch() {
		currentEpochVotes := core.NewVoteSet()
		allEpochVotes, err := e.state.GetEpochVotes()
		if err != nil {
			e.logger.WithFields(log.Fields{"err": err}).Panic("Failed to retrieve epoch votes")
		}
		for _, v := range allEpochVotes.Votes() {
			if v.Epoch >= vote.Epoch {
				currentEpochVotes.AddVote(v)
			}
		}

		if nextValidators.HasMajority(currentEpochVotes) {
			nextEpoch := vote.Epoch + 1
			endEpoch = true
			if nextEpoch > e.GetEpoch()+1 {
				// Broadcast epoch votes when jumping epoch.
				for _, v := range currentEpochVotes.Votes() {
					e.broadcastVote(v)
				}
			}

			e.logger.WithFields(log.Fields{
				"e.epoch":      e.GetEpoch,
				"nextEpoch":    nextEpoch,
				"epochVoteSet": currentEpochVotes,
			}).Debug("Majority votes for current epoch. Moving to new epoch")
			e.state.SetEpoch(nextEpoch)
		}
	}
	return
}

func (e *ConsensusEngine) checkCC(hash common.Hash) {
	if hash.IsEmpty() {
		return
	}
	block, err := e.Chain().FindBlock(hash)
	if err != nil {
		e.logger.WithFields(log.Fields{"block": hash.Hex()}).Warn("checkCC: Block hash in vote is not found")
		return
	}
	// Ingore outdated votes.
	highestCCBlockHeight := e.state.GetHighestCCBlock().Height
	if block.Height < highestCCBlockHeight {
		return
	}

	votes := e.chain.FindVotesByHash(hash)
	validators := e.validatorManager.GetValidatorSet(hash)
	if validators.HasMajority(votes) {
		e.processCCBlock(block)
	}
}

func (e *ConsensusEngine) GetTipToVote() *core.ExtendedBlock {
	return e.GetTip(true)
}

func (e *ConsensusEngine) GetTipToExtend() *core.ExtendedBlock {
	return e.GetTip(false)
}

// GetTip return the block to be extended from.
func (e *ConsensusEngine) GetTip(includePendingBlockingLeaf bool) *core.ExtendedBlock {
	hcc := e.state.GetHighestCCBlock()
	candidate := hcc

	// DFS to find valid block with the greatest height.
	stack := []*core.ExtendedBlock{candidate}
	for len(stack) > 0 {
		curr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if !curr.Status.IsValid() {
			continue
		}
		if !includePendingBlockingLeaf && curr.HasValidatorUpdate {
			// A block with validator update is newer than local HCC. Proposing
			// on this branch will violate the two direct confirmations rule for
			// blocks with validator changes.
			continue
		}

		if curr.Height > candidate.Height {
			candidate = curr
		}

		for _, childHash := range curr.Children {
			child, err := e.chain.FindBlock(childHash)
			if err != nil {
				e.logger.WithFields(log.Fields{
					"err":       err,
					"childHash": childHash.Hex(),
				}).Fatal("Failed to find child block")
			}
			stack = append(stack, child)
		}
	}
	return candidate
}

// GetSummary returns a summary of consensus state.
func (e *ConsensusEngine) GetSummary() *StateStub {
	return e.state.GetSummary()
}

// FinalizedBlocks returns a channel that will be published with finalized blocks by the engine.
func (e *ConsensusEngine) FinalizedBlocks() chan *core.Block {
	return e.finalizedBlocks
}

// GetLastFinalizedBlock returns the last finalized block.
func (e *ConsensusEngine) GetLastFinalizedBlock() *core.ExtendedBlock {
	return e.state.GetLastFinalizedBlock()
}

func (e *ConsensusEngine) processCCBlock(ccBlock *core.ExtendedBlock) {
	if ccBlock.Height <= e.state.GetHighestCCBlock().Height {
		return
	}

	e.logger.WithFields(log.Fields{"ccBlock.Hash": ccBlock.Hash().Hex(), "c.epoch": e.state.GetEpoch()}).Debug("Updating highestCCBlock")
	e.state.SetHighestCCBlock(ccBlock)
	e.chain.CommitBlock(ccBlock.Hash())

	parent, err := e.Chain().FindBlock(ccBlock.Parent)
	if err != nil {
		e.logger.WithFields(log.Fields{"err": err, "hash": ccBlock.Parent}).Error("Failed to load block")
		return
	}
	if parent.Status.IsCommitted() {
		e.finalizeBlock(parent)
	}
}

func (e *ConsensusEngine) finalizeBlock(block *core.ExtendedBlock) {
	if e.stopped {
		return
	}

	// Skip blocks that have already published.
	if block.Hash() == e.state.GetLastFinalizedBlock().Hash() {
		return
	}

	e.logger.WithFields(log.Fields{"block.Hash": block.Hash().Hex()}).Info("Finalizing block")

	e.state.SetLastFinalizedBlock(block)
	e.ledger.FinalizeState(block.Height, block.StateHash)

	// Mark block and its ancestors as finalized.
	e.chain.FinalizePreviousBlocks(block.Hash())

	// Force update TX index on block finalization so that the index doesn't point to
	// duplicate TX in fork.
	e.chain.AddTxsToIndex(block, true)

	select {
	case e.finalizedBlocks <- block.Block:
	default:
	}
}

func (e *ConsensusEngine) randHex() []byte {
	bytes := make([]byte, 10)
	e.rand.Read(bytes)
	return bytes
}

func (e *ConsensusEngine) shouldPropose(epoch uint64) bool {
	if epoch == 0 { // special handling for genesis epoch
		return false
	}
	if !e.shouldProposeByID(epoch, e.ID()) {
		return false
	}

	// Don't propose if majority has greater block height.
	tip := e.GetTipToExtend()
	epochVotes, err := e.state.GetEpochVotes()
	if err != nil {
		e.logger.WithFields(log.Fields{"error": err}).Warn("Failed to load epoch votes")
		return true
	}
	validators := e.validatorManager.GetNextValidatorSet(tip.Hash())
	votes := core.NewVoteSet()
	for _, v := range epochVotes.Votes() {
		if v.Height >= tip.Height+1 {
			votes.AddVote(v)
		}
	}
	if validators.HasMajority(votes) {
		return false
	}

	return true
}

func (e *ConsensusEngine) shouldProposeByID(epoch uint64, id string) bool {
	extBlk := e.state.GetLastFinalizedBlock()
	proposer := e.validatorManager.GetNextProposer(extBlk.Hash(), epoch)
	if proposer.ID().Hex() != id {
		return false
	}
	return true
}

func (e *ConsensusEngine) createProposal() (core.Proposal, error) {
	tip := e.GetTipToExtend()
	result := e.ledger.ResetState(tip.Height, tip.StateHash)
	if result.IsError() {
		e.logger.WithFields(log.Fields{
			"error":         result.Message,
			"tip.StateHash": tip.StateHash.Hex(),
			"tip":           tip,
		}).Panic("Failed to reset state to tip.StateHash")
	}

	// Add block.
	block := core.NewBlock()
	block.ChainID = e.chain.ChainID
	block.Epoch = e.GetEpoch()
	block.Parent = tip.Hash()
	block.Height = tip.Height + 1
	block.Proposer = e.privateKey.PublicKey().Address()
	block.Timestamp = big.NewInt(time.Now().Unix())
	block.HCC.BlockHash = e.state.GetHighestCCBlock().Hash()
	block.HCC.Votes = e.chain.FindVotesByHash(block.HCC.BlockHash).UniqueVoter()

	// Add Txs.
	newRoot, txs, result := e.ledger.ProposeBlockTxs()
	if result.IsError() {
		err := fmt.Errorf("Failed to collect Txs for block proposal: %v", result.String())
		return core.Proposal{}, err
	}
	block.AddTxs(txs)
	block.StateHash = newRoot

	// Sign block.
	sig, err := e.privateKey.Sign(block.SignBytes())
	if err != nil {
		e.logger.WithFields(log.Fields{"error": err}).Panic("Failed to sign vote")
	}
	block.SetSignature(sig)

	proposal := core.Proposal{
		Block:      block,
		ProposerID: common.HexToAddress(e.ID()),
	}

	// Add votes that might help peers progress, e.g. votes on last CC block and latest epoch
	// votes.
	lastCC := e.state.GetHighestCCBlock()
	lastCCVotes := e.chain.FindVotesByHash(lastCC.Hash())
	epochVotes, err := e.state.GetEpochVotes()
	if err != nil {
		if lastCC.Height > core.GenesisBlockHeight { // OK for the genesis block not to have votes
			e.logger.WithFields(log.Fields{"error": err}).Warn("Failed to load epoch votes")
		}
	}
	proposal.Votes = lastCCVotes.Merge(epochVotes).UniqueVoterAndBlock()
	selfVote := e.createVote(block)
	proposal.Votes.AddVote(selfVote)

	_, err = e.chain.AddBlock(block)
	if err != nil {
		return core.Proposal{}, errors.Wrap(err, "Failed to add proposed block to chain")
	}

	e.handleBlock(block)

	return proposal, nil
}

func (e *ConsensusEngine) propose() {
	var proposal core.Proposal
	var err error
	lastProposal := e.state.GetLastProposal()
	if lastProposal.Block != nil && e.GetEpoch() == lastProposal.Block.Epoch {
		proposal = lastProposal
		e.logger.WithFields(log.Fields{"proposal": proposal}).Info("Repeating proposal")
	} else {
		proposal, err = e.createProposal()
		if err != nil {
			e.logger.WithFields(log.Fields{"error": err}).Error("Failed to create proposal")
			return
		}
		e.state.LastProposal = proposal

		e.logger.WithFields(log.Fields{"proposal": proposal}).Info("Making proposal")
	}

	payload, err := rlp.EncodeToBytes(proposal)
	if err != nil {
		e.logger.WithFields(log.Fields{"proposal": proposal}).Error("Failed to encode proposal")
		return
	}
	proposalMsg := dispatcher.DataResponse{
		ChannelID: common.ChannelIDProposal,
		Payload:   payload,
	}
	e.dispatcher.SendData([]string{}, proposalMsg)
}
