//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only EXPLORATORY source-of-authority adapter. This is NOT production
// safe until concurrent cancellation, reorg, invalid FCU, and lifecycle
// boundaries have their own qualified proofs. It receives a request over
// Geth's existing authenticated Engine RPC and fetches the exact header
// corresponding to the CL-supplied finalized HASH, then records a volatile
// witness. The canonical finalized pointer/database is not modified.
package catalyst

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
)

type balV13PendingEngineAPI struct {
	*ConsensusAPI
	service *eth.Ethereum
	onAdmit func()
}

func (a *balV13PendingEngineAPI) ForkchoiceUpdatedV4(ctx context.Context, update engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes, custody *types.CustodyBitmap) (engine.ForkChoiceResponse, error) {
	bc := a.service.BlockChain()
	// Every FCU revokes the previous pending witness. A known canonical
	// finalized pointer, once set by Geth, is never changed by this observer.
	bc.SetExperimentalPendingBALFinality(nil)

	// Deliberate fail-closed envelope: non-nil payload attributes require
	// Engine API validation (fork version, withdrawals, slot, beacon root)
	// before any speculative pending authority may be admitted. We do NOT
	// duplicate Engine validation logic or pre-authorize this untested path.
	if attrs != nil {
		return a.ConsensusAPI.ForkchoiceUpdatedV4(ctx, update, attrs, custody)
	}

	// Only look up an early anchor when the head is still unknown and the
	// request explicitly named a nonzero finalized HASH. Our test controls
	// that the HTTP endpoint is JWT-authenticated, not directly callable.
	if update.HeadBlockHash != (common.Hash{}) &&
		update.FinalizedBlockHash != (common.Hash{}) &&
		bc.GetBlockByHash(update.HeadBlockHash) == nil {
		finalHeader := bc.GetHeaderByHash(update.FinalizedBlockHash)
		if finalHeader == nil {
			header, err := a.service.Downloader().GetHeader(update.FinalizedBlockHash)
			if err == nil {
				finalHeader = header
			}
		}
		if finalHeader != nil && finalHeader.Hash() == update.FinalizedBlockHash {
			// This is an ephemeral exact-hash witness, not SETFINALIZED.
			// The importer still requires authenticated hash ancestry
			// through finalizedAncestryMember before every skipped block.
			bc.SetExperimentalPendingBALFinality(finalHeader)
			if a.onAdmit != nil {
				a.onAdmit()
			}
		}
	}
	return a.ConsensusAPI.ForkchoiceUpdatedV4(ctx, update, attrs, custody)
}

func TestBALEngineV13VolatilePendingWitness(t *testing.T) {
	genesis, blocks, sender, contract := makeBALEngineV11Corpus(t)
	cases := []string{"engine-finalized-unknown", "engine-pending-finality"}
	var rows []balEngineV11Record
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			obs := runBALEngineV11(t, scenario, genesis, blocks, sender, contract)
			raw, err := json.Marshal(obs)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_BAL_ENGINE_V13_ARM %s", raw)
			rows = append(rows, obs)
		})
	}
	if len(rows) != 2 {
		t.Fatal("both independent authenticated RPC controls are required")
	}
	cold, pending := rows[0], rows[1]
	if cold.Head != pending.Head || cold.StateRoot != pending.StateRoot ||
		cold.SenderNonce != pending.SenderNonce {
		t.Fatalf("volatile pending witness changed protected state: %+v", rows)
	}
	if cold.FinalizedOnFirstSync || pending.FinalizedOnFirstSync ||
		cold.FinalizedAfterImport || pending.FinalizedAfterImport {
		t.Fatalf("experimental pending witness polluted canonical finalized state: %+v", rows)
	}
	if cold.ExecutionlessTxBlocks != 0 || pending.ExecutionlessTxBlocks == 0 ||
		pending.PendingWitnessAdmissions == 0 {
		t.Fatalf("pending-witness causal rescue failed: %+v", rows)
	}
	if cold.ValidatedBALAttachments == 0 || pending.ValidatedBALAttachments == 0 ||
		!cold.FinalizedAfterRPCReplay || !pending.FinalizedAfterRPCReplay ||
		!cold.ReopenedReceiptsMatch || !pending.ReopenedReceiptsMatch {
		t.Fatalf("BAL acquisition or Engine replay boundary failed: %+v", rows)
	}
}
