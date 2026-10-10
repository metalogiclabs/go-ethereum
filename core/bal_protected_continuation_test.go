// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
)

// TestBALProtectedFutureAcrossPebbleRestart tests the smallest previously
// unqualified continuation of finalized BAL reconstruction: persist the
// reconstructed historical state, close the blockchain and database, reopen
// Pebble, then EXECUTE a new block above finality. Both arms must reach the
// same independently generated head and state root. Unlike reconstructed
// finalized blocks, the unfinalized successor MUST retain its receipts.
//
// This is a deterministic in-process blockchain/Pebble test, not a geth
// process, CL/P2P network, shadowfork, or complete node-sync benchmark.
func TestBALProtectedFutureAcrossPebbleRestart(t *testing.T) {
	env := newBALTestEnv(types.GenesisAlloc{
		counterAddr: {Code: counterRuntime, Balance: common.Big0},
	})
	engine := beacon.New(ethash.NewFaker())
	// Include transactions on both sides of the 256-block ancestry window
	// and across the finalized / unfinalized transition at height 260/261.
	txAt := map[int]bool{0: true, 253: true, 254: true, 255: true, 256: true, 259: true, 260: true}
	_, blocks, _ := GenerateChainWithGenesis(env.gspec, engine, 261, func(i int, b *BlockGen) {
		if txAt[i] {
			b.AddTx(balTx(t, env, b.TxNonce(env.from), &counterAddr, common.Big0, nil))
		}
	})
	for _, block := range blocks {
		if block.AccessList() == nil {
			t.Fatalf("height %d has no Amsterdam access list", block.NumberU64())
		}
	}
	finalized := blocks[259]
	unfinalized := blocks[260]
	for _, reconstruct := range []bool{false, true} {
		name := "execute-all"
		if reconstruct {
			name = "reconstruct-finalized"
		}
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "pebble")
			pdb, err := pebble.New(directory, 16, 16, "", false)
			if err != nil {
				t.Fatal(err)
			}
			db := rawdb.NewDatabase(pdb)
			config := DefaultConfig().WithStateScheme(rawdb.HashScheme)
			config.BALStateReconstruction = reconstruct
			bc, err := NewBlockChain(db, env.gspec, engine, config)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			for _, block := range blocks {
				rawdb.WriteSkeletonHeader(db, block.Header())
			}
			bc.SetFinalized(finalized.Header())
			if n, err := bc.InsertChain(blocks[:260]); err != nil {
				bc.Stop()
				db.Close()
				t.Fatalf("pre-restart import %d/260: %v", n, err)
			}
			if got := bc.CurrentBlock(); got.Hash() != finalized.Hash() || got.Root != finalized.Root() {
				bc.Stop()
				db.Close()
				t.Fatal("pre-restart head or root mismatch")
			}
			historicalReceipts := 1
			if reconstruct {
				historicalReceipts = 0
			}
			if got := len(bc.GetReceiptsByHash(blocks[0].Hash())); got != historicalReceipts {
				bc.Stop()
				db.Close()
				t.Fatalf("historical receipts = %d, want %d", got, historicalReceipts)
			}
			bc.Stop()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen the very same physical Pebble directory. As a local
			// analogue of the CL forkchoice input, explicitly re-announce
			// the trusted finalized header on the new blockchain object.
			pdb, err = pebble.New(directory, 16, 16, "", false)
			if err != nil {
				t.Fatal(err)
			}
			db = rawdb.NewDatabase(pdb)
			bc, err = NewBlockChain(db, env.gspec, engine, config)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			defer db.Close()
			defer bc.Stop()
			if got := bc.CurrentBlock().Hash(); got != finalized.Hash() {
				t.Fatalf("reopened head = %s, want %s", got, finalized.Hash())
			}
			bc.SetFinalized(finalized.Header())
			if n, err := bc.InsertChain([]*types.Block{unfinalized}); err != nil {
				t.Fatalf("post-restart unfinalized import %d/1: %v", n, err)
			}
			head := bc.CurrentBlock()
			if head.Hash() != unfinalized.Hash() || head.Root != unfinalized.Root() {
				t.Fatalf("post-restart head/root = %s/%s, want %s/%s",
					head.Hash(), head.Root, unfinalized.Hash(), unfinalized.Root())
			}
			state, err := bc.State()
			if err != nil {
				t.Fatal(err)
			}
			if got := state.GetState(counterAddr, common.Hash{}).Big().Uint64(); got != uint64(len(txAt)) {
				t.Fatalf("continued state counter = %d, want %d", got, len(txAt))
			}
			if got := len(bc.GetReceiptsByHash(unfinalized.Hash())); got != 1 {
				t.Fatalf("unfinalized continuation retained %d receipts, want 1", got)
			}
			if got := len(bc.GetReceiptsByHash(blocks[0].Hash())); got != historicalReceipts {
				t.Fatalf("historical receipt boundary changed across restart: %d, want %d", got, historicalReceipts)
			}
			t.Logf("qualified %s: historical receipts=%d, unfinalized receipts=1, head=%s, root=%s",
				name, historicalReceipts, head.Hash(), head.Root)
		})
	}
}
