// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// This experiment belongs to the Metalogic branch and is overlaid onto
// ethereum/go-ethereum PR #35851 only for a bounded regression check.
package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
)

// TestBALReconstructionFinalityAncestry checks that execution can only be
// skipped for members of the finalized chain, not arbitrary blocks sharing a
// finalized height. Its fixture comes from #35851's balBlocks helper.
func TestBALReconstructionFinalityAncestry(t *testing.T) {
	env, engine, blocks, _ := balBlocks(t)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(rawdb.NewMemoryDatabase(), env.gspec, engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()

	final := blocks[len(blocks)-1]
	bc.SetFinalized(final.Header())
	if !bc.useAccessListReconstruction(final, vm.Config{}) {
		t.Fatal("finalized anchor unexpectedly ineligible")
	}
	if final.AccessList() == nil {
		t.Fatal("fixture is not Amsterdam")
	}

	// Same height as final, same body/BAL and valid state commitment,
	// but a distinct header/hash: this is a competing block, not finalized.
	siblingHeader := final.Header()
	siblingHeader.Extra = []byte("non-finalized sibling")
	sibling := final.WithSeal(siblingHeader)
	if sibling.Hash() == final.Hash() {
		t.Fatal("fixture did not create a fork")
	}
	if bc.useAccessListReconstruction(sibling, vm.Config{}) {
		t.Fatal("non-finalized sibling treated as finalized")
	}

	// A different block below finalized height is also not thereby finalized.
	earlier := blocks[len(blocks)-2]
	earlierHeader := earlier.Header()
	earlierHeader.Extra = []byte("older divergent fork")
	oldFork := earlier.WithSeal(earlierHeader)
	if oldFork.Hash() == earlier.Hash() {
		t.Fatal("earlier fixture did not create a fork")
	}
	if bc.useAccessListReconstruction(oldFork, vm.Config{}) {
		t.Fatal("non-finalized ancestor-height fork treated as finalized")
	}
}
