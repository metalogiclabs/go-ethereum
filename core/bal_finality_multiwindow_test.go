// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestBALFinalityMultiwindowInsertChain proves the checkpoint transition
// survives real block import, authenticated post-state reconstruction and
// persistence, with transactions immediately around the 256-block boundary.
func TestBALFinalityMultiwindowInsertChain(t *testing.T) {
	env := newBALTestEnv(types.GenesisAlloc{
		counterAddr: {Code: counterRuntime, Balance: common.Big0},
	})
	engine := beacon.New(ethash.NewFaker())
	txAt := map[int]bool{0: true, 253: true, 254: true, 255: true, 256: true, 259: true}
	_, blocks, _ := GenerateChainWithGenesis(env.gspec, engine, 260, func(i int, b *BlockGen) {
		if txAt[i] {
			b.AddTx(balTx(t, env, b.TxNonce(env.from), &counterAddr, common.Big0, nil))
		}
	})
	for _, block := range blocks {
		if block.AccessList() == nil {
			t.Fatalf("block %d has no Amsterdam access list", block.NumberU64())
		}
	}

	config := DefaultConfig()
	config.BALStateReconstruction = true
	db := rawdb.NewMemoryDatabase()
	bc, err := NewBlockChain(db, env.gspec, engine, config)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	// The downloader may have skeleton headers before it has inserted bodies.
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	bc.SetFinalized(blocks[len(blocks)-1].Header())
	if n, err := bc.InsertChain(blocks); err != nil {
		t.Fatalf("finalized skeleton-backed insert %d/%d: %v", n, len(blocks), err)
	}
	final := blocks[len(blocks)-1]
	if got := bc.CurrentBlock(); got.Hash() != final.Hash() || got.Root != final.Root() {
		t.Fatalf("reconstructed head %v root %v, want %v root %v", got.Hash(), got.Root, final.Hash(), final.Root())
	}
	for index := range txAt {
		if got := len(bc.GetReceiptsByHash(blocks[index].Hash())); got != 0 {
			t.Fatalf("block %d executed despite finalized skeleton proof (%d receipts)", index+1, got)
		}
	}
	s, err := bc.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.GetState(counterAddr, common.Hash{}).Big().Uint64(); got != uint64(len(txAt)) {
		t.Fatalf("reconstructed counter %d, want %d", got, len(txAt))
	}

	// Independent execution baseline must arrive at identical block/state roots,
	// but it must retain the receipts the executionless path does not produce.
	control, err := NewBlockChain(rawdb.NewMemoryDatabase(), env.gspec, engine, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Stop()
	if n, err := control.InsertChain(blocks); err != nil {
		t.Fatalf("control insert %d/%d: %v", n, len(blocks), err)
	}
	if got := control.CurrentBlock(); got.Root != final.Root() || got.Hash() != final.Hash() {
		t.Fatalf("control head %v root %v, want %v root %v", got.Hash(), got.Root, final.Hash(), final.Root())
	}
	for index := range txAt {
		if got := len(control.GetReceiptsByHash(blocks[index].Hash())); got != 1 {
			t.Fatalf("executed block %d has %d receipts, want 1", index+1, got)
		}
	}
}
