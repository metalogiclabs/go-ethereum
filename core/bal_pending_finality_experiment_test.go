//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The pending capability is distinct from persistent finalized state.
// This bounded test proves exact hash membership, missing-skeleton fallback,
// revocation, and lack of automatic restart persistence. It is NOT a formal
// guarantee against concurrent forkchoice revocation during an active import.
package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
)

func TestBALPendingFinalityV13HashAndRevocation(t *testing.T) {
	env, consensus, blocks, _ := balBlocks(t)
	db := rawdb.NewMemoryDatabase()
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(db, env.gspec, consensus, cfg)
	if err != nil {
		t.Fatal(err)
	}
	final := blocks[len(blocks)-1]
	previous := blocks[len(blocks)-2]
	empty := common.Hash{}
	if bc.CurrentFinalBlock() != nil || rawdb.ReadFinalizedBlockHash(db) != empty {
		t.Fatal("new chain has unexpected canonical finalized authority")
	}
	bc.SetExperimentalPendingBALFinality(final.Header())
	if bc.CurrentFinalBlock() != nil || rawdb.ReadFinalizedBlockHash(db) != empty {
		t.Fatal("volatile witness was incorrectly promoted to persisted finality")
	}
	if !bc.useAccessListReconstruction(final, vm.Config{}) {
		t.Fatal("exact pinned final header did not authorize matching block")
	}
	// At a lower height a header chain is required, not just a height test.
	// Without the original skeleton the verifier must fail closed.
	if bc.useAccessListReconstruction(previous, vm.Config{}) {
		t.Fatal("missing authenticated skeleton improperly authorized an ancestor")
	}
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	if !bc.useAccessListReconstruction(previous, vm.Config{}) {
		t.Fatal("exact skeleton ancestry could not establish pending membership")
	}
	// An otherwise-valid same-height sibling with the same BAL/state root
	// must not inherit finalized authority from a different block hash.
	otherHeader := final.Header()
	otherHeader.Extra = []byte("V13 nonfinalized sibling")
	sibling := final.WithSeal(otherHeader)
	if sibling.Hash() == final.Hash() || bc.useAccessListReconstruction(sibling, vm.Config{}) {
		t.Fatal("competing sibling was authorized by another finality hash")
	}
	bc.SetExperimentalPendingBALFinality(sibling.Header())
	if bc.useAccessListReconstruction(final, vm.Config{}) ||
		bc.useAccessListReconstruction(previous, vm.Config{}) {
		t.Fatal("switched pending witness allowed stale original-chain reuse")
	}
	bc.SetExperimentalPendingBALFinality(nil)
	if bc.useAccessListReconstruction(final, vm.Config{}) ||
		rawdb.ReadFinalizedBlockHash(db) != empty {
		t.Fatal("revocation failed or mutated canonical finalized state")
	}
	bc.Stop()
	other, err := NewBlockChain(db, env.gspec, consensus, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Stop()
	if other.CurrentFinalBlock() != nil ||
		other.useAccessListReconstruction(final, vm.Config{}) ||
		rawdb.ReadFinalizedBlockHash(db) != empty {
		t.Fatal("volatile pending finality leaked across blockchain reconstruction")
	}
}
