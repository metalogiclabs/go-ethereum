//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only, authenticated Engine RPC schedule experiment. It injects a
// deterministic pause into the experimental V13 pending-witness adapter.
// Neither test is a production consensus or Engine correctness certificate.
package catalyst

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
)

type balV20Harness struct {
	target     *eth.Ethereum
	first      *rpc.Client
	second     *rpc.Client
	final      *types.Block
	frozen     *types.Block
	admissions *atomic.Int64
}

func openBALV20Harness(t *testing.T, beforeAdmit func()) balV20Harness {
	t.Helper()
	genesis, blocks, _, _ := makeBALEngineV11Corpus(t)
	root := t.TempDir()
	sourceStack, source, _ := openBALEngineV11Node(t, filepath.Join(root, "source"), genesis, false, false)
	t.Cleanup(func() { sourceStack.Close() })
	n, err := source.BlockChain().InsertChain(blocks)
	if err != nil || n != len(blocks) {
		t.Fatalf("UNKNOWN/setup: source chain: %d/%d: %v", n, len(blocks), err)
	}
	admissions := new(atomic.Int64)
	factory := func(service *eth.Ethereum) rpc.API {
		return rpc.API{
			Namespace: "engine",
			Service: &balV13PendingEngineAPI{
				ConsensusAPI: NewConsensusAPI(service),
				service:      service,
				onAdmit:      func() { admissions.Add(1) },
				beforeAdmit:  beforeAdmit,
			},
			Authenticated: true,
		}
	}
	targetStack, target, secretPath := openBALEngineV11Node(
		t, filepath.Join(root, "target"), genesis, true, true, factory,
	)
	t.Cleanup(func() { targetStack.Close() })
	remote := sourceStack.Server().Self()
	if !remote.IP().IsLoopback() || remote.TCP() == 0 {
		t.Fatalf("UNKNOWN/setup: refusing nonlocal peer: %v", remote)
	}
	targetStack.Server().AddPeer(remote)
	awaitBALEngineV11(t, "authenticated TCP peer connection", 15*time.Second, func() bool {
		return sourceStack.Server().PeerCount() == 1 && targetStack.Server().PeerCount() == 1
	})
	final, frozen := blocks[len(blocks)-1], blocks[len(blocks)-2]
	awaitBALEngineV11(t, "exact peer finalized header", 20*time.Second, func() bool {
		header, err := target.Downloader().GetHeader(frozen.Hash())
		return err == nil && header != nil && header.Hash() == frozen.Hash()
	})
	secret, err := node.ObtainJWTSecret(secretPath)
	if err != nil || len(secret) != 32 {
		t.Fatalf("UNKNOWN/setup: JWT identity unavailable: %v", err)
	}
	var key [32]byte
	copy(key[:], secret)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func() *rpc.Client {
		client, err := rpc.DialOptions(ctx, targetStack.HTTPAuthEndpoint(), rpc.WithHTTPAuth(node.NewJWTAuth(key)))
		if err != nil {
			t.Fatalf("UNKNOWN/setup: authenticated RPC dial: %v", err)
		}
		t.Cleanup(client.Close)
		return client
	}
	return balV20Harness{
		target: target, first: dial(), second: dial(),
		final: final, frozen: frozen, admissions: admissions,
	}
}

func (h balV20Harness) validFCU() engine.ForkchoiceStateV1 {
	return engine.ForkchoiceStateV1{
		HeadBlockHash: h.final.Hash(), FinalizedBlockHash: h.frozen.Hash(),
	}
}

func (h balV20Harness) invalidatingFCU() engine.ForkchoiceStateV1 {
	// The V13 wrapper clears its existing speculative witness on *every*
	// FCU, including this zero-head request rejected by canonical Geth.
	return engine.ForkchoiceStateV1{FinalizedBlockHash: h.frozen.Hash()}
}

func callBALV20(t *testing.T, client *rpc.Client, fc engine.ForkchoiceStateV1) (engine.ForkChoiceResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var result engine.ForkChoiceResponse
	err := client.CallContext(ctx, &result, "engine_forkchoiceUpdatedV4", fc, nil, nil)
	return result, err
}

func TestBALV20SerialInvalidFCUClearsPending(t *testing.T) {
	h := openBALV20Harness(t, nil)
	result, err := callBALV20(t, h.first, h.validFCU())
	if err != nil || h.admissions.Load() != 1 ||
		h.target.BlockChain().ExperimentalPendingBALFinalityHash() != h.frozen.Hash() {
		t.Fatalf("UNKNOWN/control: valid FCU did not produce exact pending witness: %+v err=%v admissions=%d", result, err, h.admissions.Load())
	}
	bad, err := callBALV20(t, h.second, h.invalidatingFCU())
	if err != nil || bad.PayloadStatus.Status != engine.INVALID {
		t.Fatalf("UNKNOWN/control: zero-head FCU behavior changed: %+v err=%v", bad, err)
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) {
		t.Fatalf("UNKNOWN/control: sequential invalidation did not clear pending witness: %s", got.Hex())
	}
	t.Log("MG_BAL_V20_SERIAL_CONTROL_PASS authenticated_valid_then_invalid_clear")
}

func TestBALV21ConcurrentInvalidationSuppressesOldAdmission(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var enteredOnce sync.Once
	h := openBALV20Harness(t, func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) {
		t.Fatalf("UNKNOWN/setup: expected cold target, got %s", got.Hex())
	}
	type response struct { reply engine.ForkChoiceResponse; err error }
	firstDone := make(chan response, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var reply engine.ForkChoiceResponse
		err := h.first.CallContext(ctx, &reply, "engine_forkchoiceUpdatedV4", h.validFCU(), nil, nil)
		firstDone <- response{reply: reply, err: err}
	}()
	select {
	case <-entered:
		t.Log("MG_BAL_V21_ORDER first_authenticated_fcu_waiting_before_admission")
	case <-time.After(20*time.Second):
		t.Fatal("UNKNOWN/setup: first FCU never reached source-qualified admission point")
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) {
		t.Fatalf("UNKNOWN/setup: pending witness visible before admission: %s", got.Hex())
	}
	bad, err := callBALV20(t, h.second, h.invalidatingFCU())
	if err != nil || bad.PayloadStatus.Status != engine.INVALID {
		t.Fatalf("UNKNOWN/setup: second authenticated invalidating FCU failed: %+v err=%v", bad, err)
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) {
		t.Fatalf("UNKNOWN/setup: invalidating FCU did not leave empty witness: %s", got.Hex())
	}
	t.Log("MG_BAL_V21_ORDER second_authenticated_fcu_completed_invalidated_empty_witness")
	releaseOnce.Do(func() { close(release) })
	select {
	case first := <-firstDone:
		if first.err != nil {
			t.Fatalf("UNKNOWN/setup: first FCU did not complete after release: %+v err=%v", first.reply, first.err)
		}
	case <-time.After(20*time.Second):
		t.Fatal("UNKNOWN/setup: first FCU did not complete after release")
	}
	if admissions := h.admissions.Load(); admissions != 0 {
		t.Logf("MG_BAL_V21_OBSTRUCTION STALE_FCU_ADMITTED count=%d", admissions)
		t.Fatalf("older FCU admitted stale pending authority after newer invalidation: admissions=%d", admissions)
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != (common.Hash{}) {
		t.Logf("MG_BAL_V21_OBSTRUCTION STALE_FCU_REINSTALLED_PENDING_WITNESS pending=%s older_final=%s", got.Hex(), h.frozen.Hash().Hex())
		t.Fatalf("stale earlier FCU reinstated pending authority after later invalidation")
	}
	t.Log("MG_BAL_V21_SUPERSESSION_PASS pending_empty_after_older_fcu_completed")
}

// A newer, independently authenticated FCU with a different *valid* finalized
// hash must retain its witness when an older in-flight FCU finally returns.
// Unlike V21's invalidating B, this schedule tests both non-amplification of
// stale authority and non-suppression of the more recent valid witness.
func TestBALV22ConcurrentNewerValidWitnessPrevails(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var pauseFirst atomic.Bool
	h := openBALV20Harness(t, func() {
		if pauseFirst.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	})
	if h.final.Hash() == h.frozen.Hash() {
		t.Fatal("UNKNOWN/setup: competing finality hashes must differ")
	}
	olderDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var reply engine.ForkChoiceResponse
		olderDone <- h.first.CallContext(ctx, &reply, "engine_forkchoiceUpdatedV4", h.validFCU(), nil, nil)
	}()
	select {
	case <-entered:
		t.Log("MG_BAL_V22_ORDER older_valid_fcu_paused_before_admission")
	case <-time.After(20*time.Second):
		t.Fatal("UNKNOWN/setup: older FCU did not reach source-qualified admission")
	}
	newer := engine.ForkchoiceStateV1{HeadBlockHash: h.final.Hash(), FinalizedBlockHash: h.final.Hash()}
	result, err := callBALV20(t, h.second, newer)
	if err != nil || result.PayloadStatus.Status == engine.INVALID {
		t.Fatalf("UNKNOWN/setup: newer authenticated finality update not admissible: %+v err=%v", result, err)
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != h.final.Hash() || h.admissions.Load() != 1 {
		t.Fatalf("UNKNOWN/setup: newer qualified FCU did not install exact witness: got=%s newer=%s admissions=%d", got.Hex(), h.final.Hash().Hex(), h.admissions.Load())
	}
	t.Log("MG_BAL_V22_ORDER newer_valid_fcu_admitted_different_finalized_hash")
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-olderDone:
		if err != nil {
			t.Fatalf("UNKNOWN/setup: older FCU failed after release: %v", err)
		}
	case <-time.After(20*time.Second):
		t.Fatal("UNKNOWN/setup: older FCU did not complete")
	}
	if got := h.target.BlockChain().ExperimentalPendingBALFinalityHash(); got != h.final.Hash() || h.admissions.Load() != 1 {
		t.Logf("MG_BAL_V22_OBSTRUCTION OLD_FCU_OVERWROTE_NEW_VALID_WITNESS got=%s newer=%s admissions=%d", got.Hex(), h.final.Hash().Hex(), h.admissions.Load())
		t.Fatalf("older FCU changed the newer valid pending witness")
	}
	t.Log("MG_BAL_V22_COMPETING_VALID_PASS newer_witness_preserved_after_older_fcu_completed")
}
