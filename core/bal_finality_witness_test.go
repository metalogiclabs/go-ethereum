// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
)

// TestBALFinalityProofSequentialReuse establishes that an authenticated
// predecessor is sufficient to admit its direct successor without repeating
// a skeleton traversal, even after the original skeleton has been discarded.
func TestBALFinalityProofSequentialReuse(t *testing.T) {
	env, engine, blocks, _ := balBlocks(t)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	db := rawdb.NewMemoryDatabase()
	bc, err := NewBlockChain(db, env.gspec, engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	originalFinal := blocks[len(blocks)-1].Header()
	bc.SetFinalized(originalFinal)
	if !bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("initial finalized ancestry not verified")
	}
	if !bc.balFinalityProof.valid || bc.balFinalityProof.number != blocks[0].NumberU64() {
		t.Fatal("initial witness not retained")
	}
	for _, block := range blocks {
		rawdb.DeleteSkeletonHeader(db, block.NumberU64())
	}
	if !bc.useAccessListReconstruction(blocks[1], vm.Config{}) {
		t.Fatal("verified immediate successor should reuse retained ancestry")
	}
	if bc.balFinalityProof.number != blocks[1].NumberU64() {
		t.Fatal("sequential witness not advanced")
	}

	// A sibling at the same height must not inherit membership merely because
	// its number follows that of the verified predecessor.
	sibling := blocks[2].WithSeal(blocks[2].Header())
	hdr := blocks[2].Header()
	hdr.Extra = []byte("sibling of finalized block")
	sibling = blocks[2].WithSeal(hdr)
	if bc.useAccessListReconstruction(sibling, vm.Config{}) {
		t.Fatal("same-height sibling incorrectly inherits finality")
	}

	// Changing the finality anchor revokes the witness. With the skeleton
	// deleted, the old chain is not independently provable under the new anchor.
	newFinal := blocks[2].Header()
	newFinal.Extra = []byte("new finalized anchor")
	bc.SetFinalized(newFinal)
	if bc.useAccessListReconstruction(blocks[1], vm.Config{}) {
		t.Fatal("stale predecessor survived a finality-anchor change")
	}
	if bc.balFinalityProof.valid {
		t.Fatal("stale witness not invalidated")
	}
	bc.SetFinalized(originalFinal)
	if bc.useAccessListReconstruction(blocks[1], vm.Config{}) {
		t.Fatal("ineligible block authorized without rebuilding ancestry")
	}

	// Restoring headers earns fresh authority and allows sequential reuse.
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	if !bc.useAccessListReconstruction(blocks[0], vm.Config{}) ||
		!bc.useAccessListReconstruction(blocks[1], vm.Config{}) {
		t.Fatal("fresh chain ancestry could not be re-established")
	}
}

func TestBALFinalityProofRestartRequiresEvidence(t *testing.T) {
	env, engine, blocks, _ := balBlocks(t)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	final := blocks[len(blocks)-1].Header()
	newChain := func(withSkeleton bool) *BlockChain {
		db := rawdb.NewMemoryDatabase()
		bc, err := NewBlockChain(db, env.gspec, engine, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if withSkeleton {
			for _, block := range blocks {
				rawdb.WriteSkeletonHeader(db, block.Header())
			}
		}
		bc.SetFinalized(final)
		return bc
	}
	before := newChain(true)
	if !before.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("initial proof unavailable")
	}
	before.Stop()

	// This models process restart with lost volatile ancestry state and
	// unavailable skeleton headers; no in-memory proof can be inherited.
	restarted := newChain(false)
	defer restarted.Stop()
	if restarted.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("volatile ancestor evidence leaked across new blockchain")
	}
}
