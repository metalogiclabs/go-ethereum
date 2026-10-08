// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
)

// finalityWindowFixture creates a hash-linked synthetic header chain for
// membership tests. It is not a substitute for consensus/block-import tests.
func finalityWindowFixture(t *testing.T, depth uint64) (*BlockChain, []*types.Block) {
	t.Helper()
	env := newBALTestEnv(nil)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	db := rawdb.NewMemoryDatabase()
	bc, err := NewBlockChain(db, env.gspec, beacon.New(ethash.NewFaker()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bc.Stop)

	out := make([]*types.Block, depth)
	parent := bc.CurrentBlock().Hash()
	empty := new(bal.BlockAccessList)
	for n := uint64(1); n <= depth; n++ {
		header := &types.Header{
			ParentHash: parent,
			Number: new(big.Int).SetUint64(n),
			Difficulty: common.Big0,
			Time: n + 1000,
		}
		block := types.NewBlockWithHeader(header).WithAccessListUnsafe(empty)
		rawdb.WriteSkeletonHeader(db, header)
		out[n-1] = block
		parent = block.Hash()
	}
	bc.SetFinalized(out[len(out)-1].Header())
	return bc, out
}

func TestBALFinalityWindowRejectsForkedChild(t *testing.T) {
	bc, blocks := finalityWindowFixture(t, 3)
	if !bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("genuine first member rejected")
	}
	forkHeader := blocks[1].Header()
	forkHeader.Extra = []byte("competing child of finalized parent")
	forkedChild := blocks[1].WithSeal(forkHeader)
	if forkedChild.ParentHash() != blocks[0].Hash() {
		t.Fatal("not an immediate competing child")
	}
	if bc.useAccessListReconstruction(forkedChild, vm.Config{}) {
		t.Fatal("forked child accepted from verified-parent shortcut")
	}
	if !bc.useAccessListReconstruction(blocks[1], vm.Config{}) {
		t.Fatal("authentic finalized child rejected")
	}
}

func TestBALFinalityWindowCrossesCheckpointsAndRevokes(t *testing.T) {
	bc, blocks := finalityWindowFixture(t, 520)
	for _, n := range []uint64{1, 2, 255, 256, 257, 300} {
		if !bc.useAccessListReconstruction(blocks[n-1], vm.Config{}) {
			t.Fatalf("verified member %d rejected", n)
		}
	}
	// After the target window is loaded, mutation of the skeleton database
	// cannot change the immutable hash-derived membership decision.
	bad := blocks[299].Header()
	bad.Extra = []byte("rewritten skeleton header")
	rawdb.WriteSkeletonHeader(bc.db, bad)
	if !bc.useAccessListReconstruction(blocks[299], vm.Config{}) {
		t.Fatal("authenticated window lost its hash identity after disk mutation")
	}
	if bc.useAccessListReconstruction(blocks[299].WithSeal(bad), vm.Config{}) {
		t.Fatal("incorrect sibling promoted from active window")
	}
	// The next window has a distinct verified checkpoint. Its headers remain
	// available even though the previous window was mutated on disk.
	for _, n := range []uint64{511, 512, 513, 520} {
		if !bc.useAccessListReconstruction(blocks[n-1], vm.Config{}) {
			t.Fatalf("verified member %d rejected after checkpoint transition", n)
		}
	}
	newFinal := blocks[519].Header()
	newFinal.Extra = []byte("new finality anchor")
	bc.SetFinalized(newFinal)
	if bc.useAccessListReconstruction(blocks[299], vm.Config{}) {
		t.Fatal("proof from superseded finalized anchor was reused")
	}
}

func TestBALFinalityWindowFailsClosedOnMissingSkeleton(t *testing.T) {
	bc, blocks := finalityWindowFixture(t, 520)
	// Break the only available authenticated chain before first qualification.
	rawdb.DeleteSkeletonHeader(bc.db, 400)
	if bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("gap in finalized lineage was accepted")
	}
	if len(bc.balFinalityProof.windowHashes) != 0 {
		t.Fatal("partial proof unexpectedly promoted to a verified window")
	}
	// Restoring the exact header earns the proof, which can then be reused.
	rawdb.WriteSkeletonHeader(bc.db, blocks[399].Header())
	if !bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("restored exact finalized lineage failed to verify")
	}
}
