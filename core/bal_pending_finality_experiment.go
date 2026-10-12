//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Research-only, memory-only pending-finality authority. A per-chain epoch
// and RW fence bind a witnessed admission to the actual durable write.
// Ordinary builds have a no-authority stub; no production interface exists.
package core

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type balPendingState struct {
	sync.RWMutex
	epoch  uint64
	header *types.Header
	hash   common.Hash
}

type balPendingPermit struct {
	state     *balPendingState
	epoch     uint64
	hash      common.Hash
	canonical bool
	available bool
}

var experimentalPendingBALFinality sync.Map // *BlockChain -> *balPendingState
var experimentalBALAdmissionHooks sync.Map  // *BlockChain -> func()

func (bc *BlockChain) pendingBALState() *balPendingState {
	if raw, ok := experimentalPendingBALFinality.Load(bc); ok {
		return raw.(*balPendingState)
	}
	v, _ := experimentalPendingBALFinality.LoadOrStore(bc, new(balPendingState))
	return v.(*balPendingState)
}

// SetExperimentalPendingBALFinality rotates the epoch *under an exclusive
// lock*, synchronizing with an in-flight write-side read fence. Regranting
// the same exact header advances the epoch: stale admissions cannot survive
// an ABA revoke/regrant. nil never persists or mutates canonical finality.
func (bc *BlockChain) SetExperimentalPendingBALFinality(header *types.Header) {
	state := bc.pendingBALState()
	state.Lock()
	state.epoch++
	state.header = header
	state.hash = common.Hash{}
	if header != nil {
		state.hash = header.Hash()
	}
	state.Unlock()
}

func (bc *BlockChain) pendingBALFinality() *types.Header {
	raw, ok := experimentalPendingBALFinality.Load(bc)
	if !ok {
		return nil
	}
	s := raw.(*balPendingState)
	s.RLock()
	defer s.RUnlock()
	if s.header == nil || s.header.Hash() != s.hash {
		return nil
	}
	return s.header
}

// pendingBALAdmissionPermit snapshots the authority *before* membership is
// tested. If the authorization changes during the membership computation,
// the eventual commit fence conservatively rejects the stale snapshot.
func (bc *BlockChain) pendingBALAdmissionPermit() balPendingPermit {
	if current := bc.CurrentFinalBlock(); current != nil {
		return balPendingPermit{hash: current.Hash(), canonical: true, available: true}
	}
	raw, ok := experimentalPendingBALFinality.Load(bc)
	if !ok {
		return balPendingPermit{}
	}
	s := raw.(*balPendingState)
	s.RLock()
	defer s.RUnlock()
	if s.header == nil || s.header.Hash() != s.hash {
		return balPendingPermit{}
	}
	return balPendingPermit{state: s, epoch: s.epoch, hash: s.hash, available: true}
}

// pendingBALCommitFence is called immediately before the irreversible write,
// after the state root has been checked. Its release callback is deferred
// through the write. Any revoke/regrant must acquire the exclusive lock, so
// either the write completes first (then revoke linearizes), or this fence
// observes the changed epoch and re-executes the block.
func (bc *BlockChain) pendingBALCommitFence(token balPendingPermit) (func(), bool) {
	noop := func() {}
	if !token.available {
		return noop, false
	}
	if token.canonical {
		current := bc.CurrentFinalBlock()
		return noop, current != nil && current.Hash() == token.hash
	}
	if token.state == nil {
		return noop, false
	}
	s := token.state
	s.RLock()
	if s.epoch != token.epoch || s.header == nil ||
		s.hash != token.hash || s.header.Hash() != token.hash {
		s.RUnlock()
		return noop, false
	}
	if canonical := bc.CurrentFinalBlock(); canonical != nil && canonical.Hash() != token.hash {
		s.RUnlock()
		return noop, false
	}
	return s.RUnlock, true
}

func (bc *BlockChain) experimentalPendingBALOnStop() {
	if raw, ok := experimentalPendingBALFinality.LoadAndDelete(bc); ok {
		s := raw.(*balPendingState)
		s.Lock()
		s.epoch++
		s.header = nil
		s.hash = common.Hash{}
		s.Unlock()
	}
	experimentalBALAdmissionHooks.Delete(bc)
}

// Tagged tests can schedule an exact interleaving immediately after the
// source's ordinary admission test. This is not a live production hook.
func (bc *BlockChain) SetExperimentalBALAdmissionHook(hook func()) {
	if hook == nil {
		experimentalBALAdmissionHooks.Delete(bc)
		return
	}
	experimentalBALAdmissionHooks.Store(bc, hook)
}

func (bc *BlockChain) experimentalBALAdmissionBarrier() {
	if raw, ok := experimentalBALAdmissionHooks.Load(bc); ok {
		raw.(func())()
	}
}
