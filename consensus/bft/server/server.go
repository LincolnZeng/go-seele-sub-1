package server

import (
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/seeleteam/go-seele/consensus/bft/verifier"

	"github.com/ethereum/go-ethereum/event"
	lru "github.com/hashicorp/golang-lru"
	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/consensus"
	"github.com/seeleteam/go-seele/consensus/bft"
	BFT "github.com/seeleteam/go-seele/consensus/bft"
	bftCore "github.com/seeleteam/go-seele/consensus/bft/core"
	"github.com/seeleteam/go-seele/core/types"
	"github.com/seeleteam/go-seele/crypto"
	"github.com/seeleteam/go-seele/database"
	"github.com/seeleteam/go-seele/log"
)

type server struct {
	config       *bft.BFTConfig
	bftEventMux  *event.TypeMux
	privateKey   *ecdsa.PrivateKey
	address      common.Address
	core         bftCore.Engine
	log          *log.SeeleLog
	db           database.Database
	chain        consensus.ChainReader
	currentBlock func() *types.Block
	hasBadBlock  func(hash common.Hash) bool

	// the channels for bft engine notifications
	commitCh          chan *types.Block
	proposedBlockHash common.Hash
	sealMu            sync.Mutex
	coreStarted       bool
	coreMu            sync.RWMutex

	// Current list of candidates we are pushing
	candidates map[common.Address]bool
	// Protects the signer fields
	candidatesLock sync.RWMutex
	// Snapshots for recent block to speed up reorgs
	recents *lru.ARCCache

	// event subscription for ChainHeadEvent event
	broadcaster consensus.Broadcaster

	recentMessages *lru.ARCCache // the cache of peer's messages
	knownMessages  *lru.ARCCache // the cache of self messages

	processStep int64
}

const (
	engineTypeID = "bft"
)

/*
type Server interface {
	Address() common.Address

	// Verifiers returns the Verifier set
	Verifiers(proposal Proposal) VerifierSet

	// EventMux returns the event mux in backend
	EventMux() *event.TypeMux

	// Broadcast sends a message to all Verifiers (include self)
	Broadcast(valSet VerifierSet, payload []byte) error

	// Gossip sends a message to all Verifiers (exclude self)
	Gossip(valSet VerifierSet, payload []byte) error

	// Commit delivers an approved proposal to backend.
	// The delivered proposal will be put into blockchain.
	Commit(proposal Proposal, seals [][]byte) error

	// Verify verifies the proposal. If a consensus.ErrBlockCreateTimeOld error is returned,
	// the time difference of the proposal and current time is also returned.
	Verify(Proposal) (time.Duration, error)

	// Sign signs input data with the backend's private key
	Sign([]byte) ([]byte, error)

	// CheckSignature verifies the signature by checking if it's signed by
	// the given Verifier
	CheckSignature(data []byte, addr common.Address, sig []byte) error

	// LastProposal retrieves latest committed proposal and the address of proposer
	LastProposal() (Proposal, common.Address)

	// HasPropsal checks if the combination of the given hash and height matches any existing blocks
	HasPropsal(hash common.Hash) bool

	// GetProposer returns the proposer of the given block height
	GetProposer(height uint64) common.Address

	// ParentVerifiers returns the Verifier set of the given proposal's parent block
	ParentVerifiers(proposal Proposal) VerifierSet

	// HasBadBlock returns whether the block with the hash is a bad block
	HasBadProposal(hash common.Hash) bool
}
*/

// NeServer new a server for bft backend. This server as the engine as in the POW Algorithm
func NewServer(config *BFT.BFTConfig, privateKey *ecdsa.PrivateKey, db database.Database) consensus.Bft {
	recents, _ := lru.NewARC(inmemorySnapshots)
	recentMessages, _ := lru.NewARC(inmemoryPeers)
	knownMessages, _ := lru.NewARC(inmemoryMessages)
	server := &server{
		config:         config,
		bftEventMux:    new(event.TypeMux),
		privateKey:     privateKey,
		address:        crypto.PubkeyToAddress(privateKey.PublicKey),
		log:            log.GetLogger("bft"),
		db:             db,
		commitCh:       make(chan *types.Block, 1),
		recents:        recents,
		candidates:     make(map[common.Address]bool),
		coreStarted:    false,
		recentMessages: recentMessages,
		knownMessages:  knownMessages,
	}
	server.core = bftCore.NewCore(server, server.config)
	server.processStep = 0
	return server
}

func (s *server) Address() common.Address {
	return s.address
}

// Verifiers returns the Verifier set
func (s *server) Verifiers(proposal bft.Proposal) bft.VerifierSet {
	return s.getVerifiers(proposal.Height(), proposal.Hash())
}

// EventMux returns the event mux in backend
func (s *server) EventMux() *event.TypeMux {
	return s.bftEventMux
}

// Broadcast sends a message to all Verifiers (include self)
func (s *server) Broadcast(verSet bft.VerifierSet, payload []byte) error {
	s.processStep++
	s.log.Info("[bft-one-block] Broadcast, step %d", s.processStep)
	// fan out
	s.Gossip(verSet, payload)
	// inform self with event message
	msg := bft.MessageEvent{
		Payload: payload,
	}
	go s.bftEventMux.Post(msg)
	// fmt.Println("Post in Broadcast")
	return nil
}

// Gossip sends a message to all Verifiers (exclude self)
func (s *server) Gossip(verSet bft.VerifierSet, payload []byte) error {
	s.processStep++
	s.log.Info("[bft-one-block] Gossip, step %d", s.processStep)
	hash := crypto.HashBytes(payload)
	s.knownMessages.Add(hash, true)

	targets := make(map[common.Address]bool)
	for _, ver := range verSet.List() {
		if ver.Address() != s.Address() { // exclude self
			targets[ver.Address()] = true
		}
	}

	// send out message to all targets
	if s.broadcaster != nil && len(targets) > 0 {
		peers := s.broadcaster.FindPeers(targets)
		for addr, p := range peers {
			ms, ok := s.recentMessages.Get(addr)
			var m *lru.ARCCache
			if ok {
				m, _ := ms.(*lru.ARCCache)
				if _, alreadyHave := m.Get(hash); alreadyHave {
					continue
				}
			} else { // not ok, cache it
				m, _ = lru.NewARC(inmemoryMessages)
			}
			m.Add(hash, true)
			s.recentMessages.Add(addr, m)
			go p.Send(bftMsg, payload)

		}
	}
	return nil
}

// Commit delivers an approved proposal to backend.
// The delivered proposal will be put into blockchain.
func (s *server) Commit(proposal bft.Proposal, seals [][]byte) error {
	s.processStep++
	s.log.Info("[bft-one-block] Commit, step %d", s.processStep)
	// 1. check if the proposal is a valid block
	block, ok := proposal.(*types.Block)
	if !ok {
		s.log.Error("Invalid proposal: %v", proposal)
		return errProposalInvalid
	}
	s.log.Info("server commit a block [1] get the proposed block")

	h := block.Header

	//2. append seals into extraData
	errSeal := writeCommittedSeals(h, seals) //
	if errSeal != nil {
		return errSeal
	}
	s.log.Info("server commit a block [2] writeCommittedSeals")

	//3. then update block header
	block = block.WithSeal(h)
	s.log.Info("server commit a block [3] Committer address %s hash %s height %d", s.Address().String(), proposal.Hash().String(), proposal.Height())

	// 4-1 if the proposed and committed blocks are the same, send the proposed hash
	//   to commit channel, which is being watched inside the engine.Seal() function.
	s.proposedBlockHash = block.Hash()

	s.log.Info("server commit a block [4] s.proposedBlockHash %s ?= block.Hash() %s", s.proposedBlockHash, block.Hash())

	if s.proposedBlockHash == block.Hash() {
		s.commitCh <- block
		fmt.Println("sending a block to commit channel")
		s.log.Info("server commitCh enchannel with block %+v\n", block)
		return nil
	} else {
		s.log.Error("the proposed block is not matched, won't send block to commitCh")
	}

	// 4-2 otherwise, we try to insert the block.
	// -- if success, the ChainHeadEvent event will be broadcasted, try to build
	//    the next block and the previous Seal() will be stopped.
	// -- otherwise, a error will be returned and a round change event will be fired.
	if s.broadcaster != nil {
		s.broadcaster.Enqueue(engineTypeID, block)
	}
	return nil
}

// Verify verifies the proposal. If a consensus.ErrBlockCreateTimeOld error is returned,
// the time difference of the proposal and current time is also returned.
func (s *server) Verify(proposal bft.Proposal) (time.Duration, error) {
	s.processStep++
	s.log.Info("[bft-one-block] Verify, step %d", s.processStep)
	// 1. check proposal is a valid block
	block, ok := proposal.(*types.Block)
	if !ok {
		s.log.Error("Invalid proposal, %v", proposal)
		return 0, errProposalInvalid
	}

	// 2. check block body
	txnHash := types.MerkleRootHash(block.Transactions)
	if txnHash != block.Header.TxHash {
		return 0, errMismatchTxhashes
	}

	// 3.  verify the header of proposed block
	err := s.VerifyHeader(s.chain, block.Header)
	// ignore errEmptyCommittedSeals error because we don't have the committed seals yet
	if err == nil || err == errEmptyCommittedSeals {
		return 0, nil
	} else if err == consensus.ErrBlockCreateTimeOld {
		return time.Unix(block.Header.CreateTimestamp.Int64(), 0).Sub(now()), consensus.ErrBlockCreateTimeOld
	}
	return 0, err
}

// Sign signs input data with the backend's private key
func (s *server) Sign(data []byte) ([]byte, error) {
	s.processStep++
	s.log.Info("[bft-one-block] Sign, step %d", s.processStep)
	hashData := crypto.Keccak256([]byte(data))
	sign, err := crypto.Sign(s.privateKey, hashData)
	return sign.Sig, err
}

// CheckSignature verifies the signature by checking if it's signed by
// the given Verifier
func (s *server) CheckSignature(data []byte, addr common.Address, sig []byte) error {

	// 1. get signer
	signer, err := bft.GetSignatureAddress(data, sig)
	if err != nil {
		s.log.Error("failed to get signer with err %s", err)
		return err
	}

	// 2. compare devrived address
	if signer != addr {
		return errInvalidSignature
	}

	return nil
}

// LastProposal retrieves latest committed proposal and the address of proposer
func (s *server) LastProposal() (bft.Proposal, common.Address) {
	//
	block := s.currentBlock()
	var proposer common.Address
	if block.Height() > 0 {
		var err error
		proposer, err = s.Creator(block.Header)
		if err != nil {
			s.log.Error("failed to get block creator(proposer) with err")
			return nil, common.Address{}
		}
	}
	return block, proposer
}

// HasPropsal checks if the combination of the given hash and height matches any existing blocks
func (s *server) HasPropsal(hash common.Hash) bool {
	return s.chain.GetBlockByHash(hash) != nil
}

// GetProposer returns the proposer of the given block height
func (s *server) GetProposer(height uint64) common.Address {
	if h := s.chain.GetHeaderByHeight(height); h != nil {
		creator, _ := s.Creator(h)
		return creator
	}
	return common.Address{}
}

// ParentVerifiers returns the Verifier set of the given proposal's parent block
func (s *server) ParentVerifiers(proposal bft.Proposal) bft.VerifierSet {
	if block, ok := proposal.(*types.Block); ok {
		return s.getVerifiers(block.Height()-1, block.ParentHash())
	}
	return verifier.NewVerifierSet(nil, s.config.ProposerPolicy)
}

func (s *server) getVerifiers(height uint64, hash common.Hash) bft.VerifierSet {
	snap, err := s.snapshot(s.chain, height, hash, nil)
	if err != nil {
		return verifier.NewVerifierSet(nil, s.config.ProposerPolicy)
	}
	return snap.VerSet
}

// HasBadBlock returns whether the block with the hash is a bad block
func (s *server) HasBadProposal(hash common.Hash) bool {
	if s.hasBadBlock == nil {
		return false
	}
	return s.hasBadBlock(hash)
}
