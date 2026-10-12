//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Counterexample-preserving research: test serialized commit/revocation
// behavior independently of the first-block end-to-end corpus replay.
package core

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

func TestBALPendingFinalityV15CommitFenceSerializesRevocation(t *testing.T) {
	env, consensus, blocks, _ := balBlocks(t)
	db := rawdb.NewMemoryDatabase()
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(db, env.gspec, consensus, cfg)
	if err != nil {
		t.Fatal(err)
	}
	final := blocks[len(blocks)-1]
	bc.SetExperimentalPendingBALFinality(final.Header())
	token := bc.pendingBALAdmissionPermit()
	release, ok := bc.pendingBALCommitFence(token)
	if !ok {
		t.Fatal("fresh pending admission unexpectedly rejected")
	}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		bc.SetExperimentalPendingBALFinality(nil)
		close(done)
	}()
	<-started
	select {
	case <-done:
		t.Fatal("revoke completed while commit read fence was still held")
	case <-time.After(15 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not complete after commit fence released")
	}
	if _, ok := bc.pendingBALCommitFence(token); ok {
		t.Fatal("revoked epoch erroneously admitted a stale token")
	}
	bc.SetExperimentalPendingBALFinality(final.Header())
	if _, ok := bc.pendingBALCommitFence(token); ok {
		t.Fatal("ABA regrant of identical hash revived superseded permit")
	}
	bc.SetExperimentalPendingBALFinality(nil)
	if bc.CurrentFinalBlock() != nil ||
		rawdb.ReadFinalizedBlockHash(db) != (common.Hash{}) {
		t.Fatal("commit fence improperly mutated canonical finalized state")
	}
	bc.Stop()
	if _, ok := experimentalPendingBALFinality.Load(bc); ok {
		t.Fatal("stopped blockchain retains a keyed pending-authority object")
	}
	t.Log("MG_BAL_V15_FENCE SERIALIZED_COMMIT_AND_EPOCH_ABA")
}
