//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// V15 fork-only repair: the exact V14 counterexample is preserved on its
// original branch. This replay demands the opposite, safe behavior under
// an epoch-bound, serialized authorization-to-commit fence.
package core

import (
	"encoding/json"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

type balV14RevocationRow struct {
	Arm                    string `json:"arm"`
	RevokeBarrierCalled    bool   `json:"revoke_barrier_called"`
	CanonicalFinalityEmpty bool   `json:"canonical_finality_empty"`
	PendingRevoked         bool   `json:"pending_revoked"`
	FirstBlockReceipts     int    `json:"first_block_receipts"`
	FirstBlockTxs          int    `json:"first_block_txs"`
	ExecutedBlocks         int    `json:"executed_blocks"`
	ExecutionlessBlocks    int    `json:"executionless_blocks"`
	Head                   string `json:"head"`
	StateRoot              string `json:"state_root"`
}

func runBALV14RevokeBoundary(t *testing.T, arm string) balV14RevocationRow {
	t.Helper()
	env, consensus, blocks, calls := balBlocks(t)
	db := rawdb.NewMemoryDatabase()
	cfg := DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := NewBlockChain(db, env.gspec, consensus, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	defer bc.SetExperimentalBALAdmissionHook(nil)
	defer bc.SetExperimentalPendingBALFinality(nil)

	for _, block := range blocks {
		// This immutable contiguous authenticated skeleton is the same for
		// each arm and makes ancestry tests independent of missing headers.
		rawdb.WriteSkeletonHeader(db, block.Header())
	}
	bc.SetExperimentalPendingBALFinality(blocks[len(blocks)-1].Header())
	hit := false
	switch arm {
	case "revoked-before-admission":
		bc.SetExperimentalPendingBALFinality(nil)
	case "revoked-after-eligibility-before-write":
		bc.SetExperimentalBALAdmissionHook(func() {
			if hit {
				t.Fatal("admission barrier invoked more than once")
			}
			hit = true
			bc.SetExperimentalBALAdmissionHook(nil)
			bc.SetExperimentalPendingBALFinality(nil)
			if bc.pendingBALFinality() != nil {
				t.Fatal("pending witness failed to revoke at barrier")
			}
		})
	case "revoke-and-regrant-same-hash":
		bc.SetExperimentalBALAdmissionHook(func() {
			if hit {
				t.Fatal("ABA admission hook must fire only once")
			}
			hit = true
			bc.SetExperimentalBALAdmissionHook(nil)
			bc.SetExperimentalPendingBALFinality(nil)
			bc.SetExperimentalPendingBALFinality(blocks[len(blocks)-1].Header())
		})
	default:
		t.Fatalf("unknown arm %q", arm)
	}
	inserted, err := bc.InsertChain(blocks)
	if err != nil || inserted != len(blocks) {
		t.Fatalf("%s inserted %d/%d: %v", arm, inserted, len(blocks), err)
	}
	// Revoke the ABA witness after the full import, not during it, so only
	// the first-block stale-token check is at issue in that arm.
	if arm == "revoke-and-regrant-same-hash" {
		bc.SetExperimentalPendingBALFinality(nil)
	}
	final := blocks[len(blocks)-1]
	gotHead := bc.CurrentBlock()
	if gotHead.Hash() != final.Hash() || gotHead.Root != final.Root() {
		t.Fatalf("%s protected final head/root mismatch", arm)
	}
	state, err := bc.State()
	if err != nil {
		t.Fatal(err)
	}
	if actual := state.GetState(counterAddr, common.Hash{}).Big().Int64(); actual != int64(calls) {
		t.Fatalf("%s final contract counter=%d want=%d", arm, actual, calls)
	}
	row := balV14RevocationRow{
		Arm:                    arm,
		RevokeBarrierCalled:    hit,
		PendingRevoked:         bc.pendingBALFinality() == nil,
		CanonicalFinalityEmpty: bc.CurrentFinalBlock() == nil && rawdb.ReadFinalizedBlockHash(db) == (common.Hash{}),
		FirstBlockTxs:          len(blocks[0].Transactions()),
		FirstBlockReceipts:     len(bc.GetReceiptsByHash(blocks[0].Hash())),
		Head:                   final.Hash().Hex(),
		StateRoot:              final.Root().Hex(),
	}
	for _, block := range blocks {
		n := len(bc.GetReceiptsByHash(block.Hash()))
		switch n {
		case 0:
			row.ExecutionlessBlocks++
		case len(block.Transactions()):
			row.ExecutedBlocks++
		default:
			t.Fatalf("%s unexpected receipts at #%d: %d", arm, block.NumberU64(), n)
		}
	}
	if !row.PendingRevoked || !row.CanonicalFinalityEmpty ||
		row.ExecutionlessBlocks+row.ExecutedBlocks != len(blocks) {
		t.Fatalf("control state invalid: %+v", row)
	}
	return row
}

func TestBALPendingFinalityV15FenceRejectsRevokedAndABAPermits(t *testing.T) {
	var rows []balV14RevocationRow
	for _, arm := range []string{"revoked-before-admission", "revoked-after-eligibility-before-write", "revoke-and-regrant-same-hash"} {
		t.Run(arm, func(t *testing.T) {
			r := runBALV14RevokeBoundary(t, arm)
			bytes, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_BAL_V15_ARM %s", bytes)
			rows = append(rows, r)
		})
	}
	if len(rows) != 3 {
		t.Fatalf("wanted three isolated arms, got %d", len(rows))
	}
	before, after, aba := rows[0], rows[1], rows[2]
	if before.RevokeBarrierCalled || before.ExecutionlessBlocks != 0 ||
		before.FirstBlockReceipts != before.FirstBlockTxs {
		t.Fatalf("quiescent revocation control did not execute every block: %+v", before)
	}
	if !after.RevokeBarrierCalled || after.ExecutionlessBlocks != 0 ||
		after.ExecutedBlocks != 3 || after.FirstBlockReceipts != after.FirstBlockTxs {
		t.Fatalf("revoked V14 permit was not fenced and re-executed: %+v", after)
	}
	if !aba.RevokeBarrierCalled || aba.FirstBlockReceipts != aba.FirstBlockTxs {
		t.Fatalf("ABA same-hash regrant incorrectly revived the old permit: %+v", aba)
	}
	if before.Head != after.Head || before.StateRoot != after.StateRoot ||
		before.Head != aba.Head || before.StateRoot != aba.StateRoot {
		t.Fatalf("authorization interleaving changed protected outcome: %+v", rows)
	}
	t.Log("MG_BAL_V15_VERDICT EPOCH_FENCE_REEXECUTES_REVOKED_AND_ABA")
}
