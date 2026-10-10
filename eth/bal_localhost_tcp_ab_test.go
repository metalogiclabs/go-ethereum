// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only test: two actual Geth nodes connected over localhost TCP.
// No discovery, bootnodes, public peers, external consensus or upstream edits.
package eth

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

type balTCPObservation struct {
	Round                         int    `json:"round"`
	Mode                          string `json:"mode"`
	NegotiatedETH                 uint   `json:"negotiated_eth"`
	LocalSocketPeers              int    `json:"local_socket_peers"`
	Head                          string `json:"head"`
	StateRoot                     string `json:"state_root"`
	Counter                       uint64 `json:"counter"`
	Blocks                        int    `json:"blocks"`
	TxBlocks                      int    `json:"tx_blocks"`
	ExecutedTxBlocks              int    `json:"executed_tx_blocks"`
	ExecutionlessTxBlocks         int    `json:"executionless_tx_blocks"`
	ReopenedExecutedTxBlocks      int    `json:"reopened_executed_tx_blocks"`
	ReopenedExecutionlessTxBlocks int    `json:"reopened_executionless_tx_blocks"`
	SyncWallNS                    int64  `json:"sync_wall_ns"`
	RestartVerified               bool   `json:"restart_verified"`
}

func balTCPWait(t *testing.T, label string, limit time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", label)
}

// balTCPOneArm takes the same fixed Amsterdam corpus, separately creates a
// serving and importing Geth node, then explicitly dials the serving node's
// loopback-only enode through the normal encrypted devp2p TCP transport.
// All imported blocks must come through the real Geth downloader.
func balTCPOneArm(t *testing.T, round int, reconstruct bool, genesis *core.Genesis, blocks []*types.Block, contract common.Address, txCount int) balTCPObservation {
	t.Helper()
	mode := "execute"
	if reconstruct {
		mode = "reconstruct"
	}
	base := t.TempDir()
	sourceStack, source := openBALNodeAB(t, filepath.Join(base, "source"), genesis, false)
	if n, err := source.BlockChain().InsertChain(blocks); n != len(blocks) || err != nil {
		sourceStack.Close()
		t.Fatalf("source import %d/%d: %v", n, len(blocks), err)
	}
	if len(source.BlockChain().GetAccessListRLP(blocks[0].Hash())) == 0 {
		t.Fatal("source missing first authenticated BAL")
	}

	targetDir := filepath.Join(base, "target")
	targetStack, target := openBALNodeAB(t, targetDir, genesis, reconstruct)
	final := blocks[len(blocks)-1]
	target.BlockChain().SetFinalized(final.Header())
	balTCPWait(t, "source range at final head", 8*time.Second, func() bool {
		return source.handler.blockRange.currentRange().LatestBlock >= final.NumberU64()
	})

	// Self() advertises only the local loopback interface in this fixture:
	// node.Config uses P2P.ListenAddr=127.0.0.1:0 and NoDiscovery=true.
	remote := sourceStack.Server().Self()
	if !remote.IP().IsLoopback() || remote.TCP() == 0 {
		t.Fatalf("refusing non-loopback or portless test peer: %s", remote.String())
	}
	targetStack.Server().AddPeer(remote)
	balTCPWait(t, "encrypted loopback TCP ETH peer registration", 20*time.Second, func() bool {
		return targetStack.Server().PeerCount() == 1 &&
			sourceStack.Server().PeerCount() == 1 &&
			target.handler.peers.len() == 1 &&
			source.handler.peers.len() == 1
	})
	negotiated := uint(0)
	for _, p := range target.handler.peers.all() {
		if v := p.Version(); v > negotiated {
			negotiated = v
		}
	}
	if negotiated < eth.ETH71 {
		t.Fatalf("negotiated ETH/%d cannot serve BAL packets", negotiated)
	}

	start := time.Now()
	if err := target.handler.downloader.BeaconSync(final.Header(), final.Header()); err != nil {
		t.Fatalf("TCP peer BeaconSync: %v", err)
	}
	balTCPWait(t, "real socket peer downloader completes", 65*time.Second, func() bool {
		return target.BlockChain().CurrentBlock().Hash() == final.Hash()
	})
	syncElapsed := time.Since(start)
	executed, skipped := balWireCheck(t, target.BlockChain(), blocks, contract, txCount)
	obs := balTCPObservation{
		Round: round, Mode: mode, NegotiatedETH: negotiated,
		LocalSocketPeers: targetStack.Server().PeerCount(),
		Head:             final.Hash().Hex(), StateRoot: final.Root().Hex(),
		Counter: uint64(txCount), Blocks: len(blocks), TxBlocks: txCount,
		ExecutedTxBlocks: executed, ExecutionlessTxBlocks: skipped,
		SyncWallNS: syncElapsed.Nanoseconds(),
	}
	if obs.LocalSocketPeers != 1 {
		t.Fatalf("TCP peer disconnected during qualification: %+v", obs)
	}
	if !reconstruct && skipped != 0 {
		t.Fatalf("execute arm improperly skipped tx execution: %+v", obs)
	}
	if err := targetStack.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sourceStack.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStack, reopened := openBALNodeAB(t, targetDir, genesis, reconstruct)
	executedAfter, skippedAfter := balWireCheck(t, reopened.BlockChain(), blocks, contract, txCount)
	if err := reopenedStack.Close(); err != nil {
		t.Fatal(err)
	}
	obs.ReopenedExecutedTxBlocks = executedAfter
	obs.ReopenedExecutionlessTxBlocks = skippedAfter
	obs.RestartVerified = executedAfter == executed && skippedAfter == skipped
	if !obs.RestartVerified {
		t.Fatalf("persisted receipt partition changed: %+v", obs)
	}
	return obs
}

// Paired TCP tests use identical source/target configurations except for
// BALStateReconstruction. All traffic stays on loopback; wall time is not a
// mainnet/P2P-wide performance claim and receipt equivalence is not expected.
func TestBALRealLoopbackTCPDownloaderPaired(t *testing.T) {
	genesis, blocks, contract, txCount := makeBALNodeABCorpus(t)
	for round := 0; round < 2; round++ {
		order := []bool{false, true}
		if round == 1 {
			order = []bool{true, false}
		}
		var pair [2]balTCPObservation
		for _, reconstruct := range order {
			r := balTCPOneArm(t, round, reconstruct, genesis, blocks, contract, txCount)
			if reconstruct {
				pair[1] = r
			} else {
				pair[0] = r
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_ETH_TCP_EVIDENCE %s", data)
		}
		if pair[0].Head != pair[1].Head || pair[0].StateRoot != pair[1].StateRoot || pair[0].Counter != pair[1].Counter {
			t.Fatalf("pair %d has divergent protected state", round)
		}
		t.Logf("MG_ETH_TCP_PAIR %s", fmt.Sprintf("round=%d execute_ns=%d reconstruct_ns=%d skipped=%d", round, pair[0].SyncWallNS, pair[1].SyncWallNS, pair[1].ExecutionlessTxBlocks))
	}
}
