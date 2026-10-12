//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only negative separator. This test is deliberately designed to pass
// when the unfenced V13 implementation exhibits the unsafe old-permit commit.
// Do not turn this into a production contract or conflate a matching state
// root with authorization to omit historical transaction execution.
package core

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestBALPendingV14RevokedWhileReconstructionInFlight(t *testing.T) {
	env, consensus, blocks, _ := balBlocks(t)
	db := rawdb.NewMemoryDatabase()
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(db, env.gspec, consensus, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()

	// Exact contiguous skeleton authority allows the same 3-block fixture
	// that passed the V13 quiescent ancestry and restart controls.
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	bc.SetExperimentalPendingBALFinality(blocks[len(blocks)-1].Header())
	defer bc.SetExperimentalPendingBALFinality(nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var once sync.Once
	bc.SetExperimentalBALBeforeCommitHook(func(block *types.Block) {
		if block.Hash() != blocks[0].Hash() {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	})
	defer bc.SetExperimentalBALBeforeCommitHook(nil)
	type result struct {
		n int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := bc.InsertChain(blocks)
		done <- result{n: n, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("import never reached the post-root/pre-commit hook")
	}
	// This is a true in-flight revocation, AFTER eligibility and root
	// verification but BEFORE the irreversible state/receipt write.
	bc.SetExperimentalPendingBALFinality(nil)
	if bc.pendingBALFinality() != nil || bc.CurrentFinalBlock() != nil ||
		rawdb.ReadFinalizedBlockHash(db) != (common.Hash{}) {
		t.Fatal("revocation unexpectedly retained pending/canonical finality")
	}
	close(release)
	select {
	case out := <-done:
		if out.err != nil || out.n != len(blocks) {
			t.Fatalf("import after revocation failed unexpectedly: %d/%d %v", out.n, len(blocks), out.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("import did not terminate after revocation and release")
	}
	got := bc.GetReceiptsByHash(blocks[0].Hash())
	if len(got) != 0 {
		t.Fatalf("V13 baseline race no longer exhibits a stale executionless commit: receipts=%d", len(got))
	}
	if bc.CurrentBlock().Root != blocks[len(blocks)-1].Root() {
		t.Fatal("baseline witness changed final state root")
	}
	// Explicitly retain the negative result in CI logs and in the lineage.
	t.Logf("MG_BAL_V14_STALE_AUTHORIZATION %s",
		fmt.Sprintf("revoked_before_commit=true canonical_finality=nil block=%d receipts=%d committed_executionless=true", blocks[0].NumberU64(), len(got)))
}
