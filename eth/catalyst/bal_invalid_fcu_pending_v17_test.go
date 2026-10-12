//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only V17 qualification: malformed authenticated Engine V4 requests
// must never grant a pending finalized-state capability. The exact same
// validation helper is used by regular Geth and the tagged test adapter.
package catalyst

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
)

type balV17PrevalidationObservation struct {
	RejectedVariants              []string `json:"rejected_variants"`
	PendingAdmissions             int64    `json:"pending_admissions"`
	PendingHashAfterRejections    string   `json:"pending_hash_after_rejections"`
	CanonicalFinalityStillEmpty   bool     `json:"canonical_finality_still_empty"`
	NoBlockImportStarted          bool     `json:"no_block_import_started"`
	ZeroHeadInvalidRequestCleared bool     `json:"zero_head_invalid_request_cleared"`
}

func TestBALInvalidFCUV17RejectedBeforePendingAdmission(t *testing.T) {
	genesis, blocks, _, _ := makeBALEngineV11Corpus(t)
	root := t.TempDir()
	sourceStack, source, _ := openBALEngineV11Node(t, filepath.Join(root, "source"), genesis, false, false)
	defer sourceStack.Close()
	if n, err := source.BlockChain().InsertChain(blocks); err != nil || n != len(blocks) {
		t.Fatalf("source chain import %d/%d: %v", n, len(blocks), err)
	}
	var admissions atomic.Int64
	factory := func(service *eth.Ethereum) rpc.API {
		return rpc.API{
			Namespace: "engine",
			Service: &balV13PendingEngineAPI{
				ConsensusAPI: NewConsensusAPI(service),
				service:      service,
				onAdmit:      func() { admissions.Add(1) },
			},
			Authenticated: true,
		}
	}
	targetStack, target, jwtFile := openBALEngineV11Node(
		t, filepath.Join(root, "target"), genesis, true, true, factory,
	)
	defer targetStack.Close()
	remote := sourceStack.Server().Self()
	if !remote.IP().IsLoopback() || remote.TCP() == 0 {
		t.Fatalf("refusing nonlocal source %v", remote)
	}
	targetStack.Server().AddPeer(remote)
	awaitBALEngineV11(t, "connected authenticated Geth TCP peer", 15*time.Second, func() bool {
		return sourceStack.Server().PeerCount() == 1 && targetStack.Server().PeerCount() == 1
	})
	final := blocks[len(blocks)-1]
	frozen := blocks[len(blocks)-2]
	awaitBALEngineV11(t, "exact finalized header retrievable", 20*time.Second, func() bool {
		header, err := target.Downloader().GetHeader(frozen.Hash())
		return err == nil && header != nil && header.Hash() == frozen.Hash()
	})

	secret, err := node.ObtainJWTSecret(jwtFile)
	if err != nil || len(secret) != 32 {
		t.Fatalf("missing Engine JWT identity: %v", err)
	}
	var key [32]byte
	copy(key[:], secret)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := rpc.DialOptions(ctx, targetStack.HTTPAuthEndpoint(), rpc.WithHTTPAuth(node.NewJWTAuth(key)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	before := target.BlockChain().CurrentBlock().Hash()
	var beaconRoot common.Hash
	var slot uint64 = 1
	_ = slot
	timestamp := final.Time() + 12
	variants := []struct {
		name  string
		attrs *engine.PayloadAttributes
	}{
		{"missing-withdrawals", &engine.PayloadAttributes{Timestamp: timestamp}},
		{"missing-beacon-root", &engine.PayloadAttributes{
			Timestamp: timestamp, Withdrawals: []*types.Withdrawal{},
		}},
		{"missing-slot-number", &engine.PayloadAttributes{
			Timestamp: timestamp, Withdrawals: []*types.Withdrawal{}, BeaconRoot: &beaconRoot,
		}},
	}
	fc := engine.ForkchoiceStateV1{
		HeadBlockHash:      final.Hash(),
		FinalizedBlockHash: frozen.Hash(),
	}
	observed := balV17PrevalidationObservation{}
	for _, variant := range variants {
		var response engine.ForkChoiceResponse
		err := client.CallContext(ctx, &response, "engine_forkchoiceUpdatedV4", fc, variant.attrs, nil)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "payload attributes") {
			t.Fatalf("%s: malformed FCU did not fail at Engine attribute validation: %+v %v", variant.name, response, err)
		}
		if got := target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) || admissions.Load() != 0 {
			t.Fatalf("%s: rejected FCU granted pending BAL authority %s, admissions %d", variant.name, got, admissions.Load())
		}
		if target.BlockChain().CurrentBlock().Hash() != before ||
			target.BlockChain().CurrentFinalBlock() != nil ||
			rawdb.ReadFinalizedBlockHash(target.ChainDb()) != (common.Hash{}) {
			t.Fatalf("%s: invalid Engine call changed canonical state", variant.name)
		}
		observed.RejectedVariants = append(observed.RejectedVariants, variant.name)
	}
	// Zero-head FCU remains invalid and cannot recreate the previous witness.
	var zeroReply engine.ForkChoiceResponse
	err = client.CallContext(ctx, &zeroReply, "engine_forkchoiceUpdatedV4",
		engine.ForkchoiceStateV1{FinalizedBlockHash: frozen.Hash()}, nil, nil)
	if err != nil || zeroReply.PayloadStatus.Status != engine.INVALID {
		t.Fatalf("zero-head rejection changed: %+v, %v", zeroReply, err)
	}
	observed.PendingAdmissions = admissions.Load()
	observed.PendingHashAfterRejections = target.BlockChain().ExperimentalPendingBALFinalityHash().Hex()
	observed.CanonicalFinalityStillEmpty = target.BlockChain().CurrentFinalBlock() == nil &&
		rawdb.ReadFinalizedBlockHash(target.ChainDb()) == (common.Hash{})
	observed.NoBlockImportStarted = target.BlockChain().CurrentBlock().Hash() == before
	observed.ZeroHeadInvalidRequestCleared = target.BlockChain().ExperimentalPendingBALFinalityHash() == (common.Hash{})
	if len(observed.RejectedVariants) != 3 || observed.PendingAdmissions != 0 ||
		observed.PendingHashAfterRejections != (common.Hash{}).Hex() ||
		!observed.CanonicalFinalityStillEmpty || !observed.NoBlockImportStarted ||
		!observed.ZeroHeadInvalidRequestCleared {
		t.Fatalf("V17 pending authority admission violated: %+v", observed)
	}
	payload, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MG_BAL_V17_ARM %s", payload)
}
