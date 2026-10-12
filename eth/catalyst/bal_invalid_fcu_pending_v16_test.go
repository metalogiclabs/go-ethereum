//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only V17 conservative repair. V16's negative evidence is preserved
// unchanged on its source branch. A green test here means rejected V4
// payload attributes did NOT create pending finality, not general Engine safety.
package catalyst

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
)

type balV16InvalidFCUWitness struct {
	InvalidRPCRejected            bool   `json:"invalid_rpc_rejected"`
	ErrorText                     string `json:"error_text"`
	PendingHashAfterInvalidRPC    string `json:"pending_hash_after_invalid_rpc"`
	AnnouncedFinalizedHash        string `json:"announced_finalized_hash"`
	PendingWitnessAdmissions      int64  `json:"pending_witness_admissions"`
	CanonicalFinalizedStateEmpty  bool   `json:"canonical_finalized_state_empty"`
	NoBlockImportStarted          bool   `json:"no_block_import_started"`
	ZeroHeadInvalidRequestCleared bool   `json:"zero_head_invalid_request_cleared"`
}

func TestBALInvalidFCUV17MalformedAttributesCannotAdmitPendingAuthority(t *testing.T) {
	genesis, blocks, _, _ := makeBALEngineV11Corpus(t)
	root := t.TempDir()
	sourceStack, source, _ := openBALEngineV11Node(t, filepath.Join(root, "source"), genesis, false, false)
	defer sourceStack.Close()
	n, err := source.BlockChain().InsertChain(blocks)
	if err != nil || n != len(blocks) {
		t.Fatalf("source chain: %d/%d: %v", n, len(blocks), err)
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
	targetStack, target, secretPath := openBALEngineV11Node(
		t, filepath.Join(root, "target"), genesis, true, true, factory,
	)
	defer targetStack.Close()
	remote := sourceStack.Server().Self()
	if !remote.IP().IsLoopback() || remote.TCP() == 0 {
		t.Fatalf("refusing external source %v", remote)
	}
	targetStack.Server().AddPeer(remote)
	awaitBALEngineV11(t, "authenticated TCP peer connection", 15*time.Second, func() bool {
		return sourceStack.Server().PeerCount() == 1 && targetStack.Server().PeerCount() == 1
	})
	final := blocks[len(blocks)-1]
	frozen := blocks[len(blocks)-2]
	// Confirm that this real Geth peer can serve the exact finalized header.
	// This avoids mistaking a peer-registration failure for sound behavior.
	awaitBALEngineV11(t, "available exact finalized header", 20*time.Second, func() bool {
		header, err := target.Downloader().GetHeader(frozen.Hash())
		return err == nil && header != nil && header.Hash() == frozen.Hash()
	})
	secret, err := node.ObtainJWTSecret(secretPath)
	if err != nil || len(secret) != 32 {
		t.Fatalf("JWT identity unavailable: %v", err)
	}
	var key [32]byte
	copy(key[:], secret)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := rpc.DialOptions(ctx, targetStack.HTTPAuthEndpoint(), rpc.WithHTTPAuth(node.NewJWTAuth(key)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	before := target.BlockChain().CurrentBlock().Hash()
	bad := &engine.PayloadAttributes{Timestamp: final.Time() + 12}
	fc := engine.ForkchoiceStateV1{
		HeadBlockHash:      final.Hash(),
		FinalizedBlockHash: frozen.Hash(),
	}
	var reply engine.ForkChoiceResponse
	rejectErr := client.CallContext(ctx, &reply, "engine_forkchoiceUpdatedV4", fc, bad, nil)
	if rejectErr == nil {
		t.Fatalf("Engine V4 unexpectedly accepted missing withdrawals/slotNumber: %+v", reply)
	}
	if !strings.Contains(strings.ToLower(rejectErr.Error()), "payload attributes") &&
		!strings.Contains(strings.ToLower(rejectErr.Error()), "withdrawals") {
		t.Fatalf("unexpected malformed-FCU rejection: %v", rejectErr)
	}

	observed := balV16InvalidFCUWitness{
		InvalidRPCRejected:         true,
		ErrorText:                  rejectErr.Error(),
		PendingHashAfterInvalidRPC: target.BlockChain().ExperimentalPendingBALFinalityHash().Hex(),
		AnnouncedFinalizedHash:     frozen.Hash().Hex(),
		PendingWitnessAdmissions:   admissions.Load(),
		CanonicalFinalizedStateEmpty: target.BlockChain().CurrentFinalBlock() == nil &&
			rawdb.ReadFinalizedBlockHash(target.ChainDb()) == (common.Hash{}),
		NoBlockImportStarted: target.BlockChain().CurrentBlock().Hash() == before,
	}
	if !observed.CanonicalFinalizedStateEmpty || !observed.NoBlockImportStarted {
		t.Fatalf("test lost cold-state control: %+v", observed)
	}
	// A second invalid FCU with zero head is explicitly rejected by Geth.
	// The wrapper does remove its earlier pending witness, confirming that
	// the invalid-attributes window was a real retained authority.
	var zeroReply engine.ForkChoiceResponse
	err = client.CallContext(ctx, &zeroReply, "engine_forkchoiceUpdatedV4",
		engine.ForkchoiceStateV1{FinalizedBlockHash: frozen.Hash()}, nil, nil)
	if err != nil || zeroReply.PayloadStatus.Status != engine.INVALID {
		t.Fatalf("zero-head FCU control got %+v, %v", zeroReply, err)
	}
	observed.ZeroHeadInvalidRequestCleared =
		target.BlockChain().ExperimentalPendingBALFinalityHash() == (common.Hash{})
	if !observed.ZeroHeadInvalidRequestCleared ||
		observed.PendingHashAfterInvalidRPC != (common.Hash{}).Hex() ||
		observed.PendingWitnessAdmissions != 0 {
		t.Fatalf("V17 invalid-attributes guard did not fail closed: %+v", observed)
	}
	bytes, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MG_BAL_V17_ARM %s", bytes)
	t.Log(fmt.Sprintf("MG_BAL_V17_VERDICT %s", "MALFORMED_V4_NO_PENDING_AUTHORITY"))
}
