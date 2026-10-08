// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
)

// TestBALReconstructionFromSkeletonHeaders requires authenticated ancestry to
// be usable while block headers reside in the downloader's skeleton store,
// before they have been imported into the canonical HeaderChain.
func TestBALReconstructionFromSkeletonHeaders(t *testing.T) {
	env, engine, blocks, _ := balBlocks(t)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(rawdb.NewMemoryDatabase(), env.gspec, engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()

	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(bc.db, block.Header())
	}
	bc.SetFinalized(blocks[len(blocks)-1].Header())
	if !bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("authenticated finalized skeleton ancestor unavailable to reconstruction")
	}
	if n, err := bc.InsertChain(blocks); err != nil {
		t.Fatalf("import %d/%d: %v", n, len(blocks), err)
	}
	for _, block := range blocks {
		if got := len(bc.GetReceiptsByHash(block.Hash())); got != 0 {
			t.Fatalf("finalized skeleton-backed block #%d executed: %d receipts", block.NumberU64(), got)
		}
	}
}

// TestBALReconstructionRejectsBrokenSkeletonChain proves that matching a
// header at the candidate height is insufficient: every parent hash must
// lead to the finalized anchor. A rewritten intermediate skeleton header
// cannot authorize execution avoidance.
func TestBALReconstructionRejectsBrokenSkeletonChain(t *testing.T) {
	env, engine, blocks, _ := balBlocks(t)
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(rawdb.NewMemoryDatabase(), env.gspec, engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()

	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(bc.db, block.Header())
	}
	bc.SetFinalized(blocks[len(blocks)-1].Header())
	middle := blocks[1].Header()
	middle.Extra = []byte("wrong intermediate skeleton header")
	rawdb.WriteSkeletonHeader(bc.db, middle)

	if bc.useAccessListReconstruction(blocks[0], vm.Config{}) {
		t.Fatal("broken skeleton ancestry authorized executionless import")
	}
	if bc.useAccessListReconstruction(blocks[len(blocks)-1], vm.Config{}) == false {
		t.Fatal("exact finalized anchor unexpectedly ineligible")
	}
}
