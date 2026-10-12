//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// VOLATILE FORK RESEARCH ONLY. Never persist or claim a canonical finalized
// head from this map. The caller is responsible for authenticating the exact
// consensus finalized hash. The normal build omits this setter entirely.
package core

import (
	"sync"

	"github.com/ethereum/go-ethereum/core/types"
)

var experimentalPendingBALFinality sync.Map // *BlockChain -> *types.Header

// SetExperimentalPendingBALFinality is an opt-in, memory-only research witness.
// A nil value revokes the witness; no canonical database or finalized pointer
// is updated. Hash ancestry is checked separately for EVERY candidate block.
func (bc *BlockChain) SetExperimentalPendingBALFinality(header *types.Header) {
	if header == nil {
		experimentalPendingBALFinality.Delete(bc)
		return
	}
	experimentalPendingBALFinality.Store(bc, header)
}

func (bc *BlockChain) pendingBALFinality() *types.Header {
	raw, ok := experimentalPendingBALFinality.Load(bc)
	if !ok {
		return nil
	}
	return raw.(*types.Header)
}
