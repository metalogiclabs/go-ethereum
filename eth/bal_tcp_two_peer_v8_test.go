//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only two-peer, encrypted TCP experiment. It exercises one clean
// BAL-serving peer and optionally a second ETH/72 peer with no BAL records.
// The same generated finalized chain, network impairment, and protected
// postconditions are used in all arms. This does not simulate a CL client.
package eth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/node"
)

type balTwoPeerObservation struct {
	DelayMS        int     `json:"delay_ms"`
	Scenario       string  `json:"scenario"`
	Trial          int     `json:"trial"`
	Mode           string  `json:"mode"`
	PeerCount      int     `json:"peer_count"`
	MinimumETH     uint    `json:"minimum_eth"`
	MissingBALPeer bool    `json:"missing_bal_peer"`
	Head           string  `json:"head"`
	StateRoot      string  `json:"state_root"`
	Counter        uint64  `json:"counter"`
	Executed       int     `json:"executed_tx_blocks"`
	Executionless  int     `json:"executionless_tx_blocks"`
	ReopenedExec   int     `json:"reopened_executed_tx_blocks"`
	ReopenedSkip   int     `json:"reopened_executionless_tx_blocks"`
	WallNS         int64   `json:"sync_wall_ns"`
	GraceChargedMS float64 `json:"grace_charged_ms"`
	RestartOK      bool    `json:"restart_verified"`
}

func runBALTwoPeerArm(t *testing.T, trial int, delay int, scenario string, mode string, genesis *core.Genesis, blocks []*types.Block, contract common.Address, txCount int) balTwoPeerObservation {
	t.Helper()
	base := t.TempDir()
	healthy := scenario == "two-healthy"
	missing := scenario == "one-missing-bal"
	if !healthy && !missing {
		t.Fatalf("unsupported two-peer scenario %q", scenario)
	}
	reconstruct := mode != "execute"
	grace := mode == "reconstruct-grace"
	if mode != "execute" && mode != "reconstruct-immediate" && !grace {
		t.Fatalf("unsupported mode %q", mode)
	}

	var stacks [2]*node.Node
	var services [2]*Ethereum
	defer func() {
		for _, stack := range stacks {
			if stack != nil {
				_ = stack.Close()
			}
		}
	}()
	final := blocks[len(blocks)-1]
	for i := range stacks {
		stacks[i], services[i] = openBALNodeAB(t, filepath.Join(base, fmt.Sprintf("source-%d", i)), genesis, false)
		n, err := services[i].BlockChain().InsertChain(blocks)
		if err != nil || n != len(blocks) {
			t.Fatalf("source %d import %d/%d: %v", i, n, len(blocks), err)
		}
		if got := services[i].BlockChain().GetAccessListRLP(blocks[0].Hash()); len(got) == 0 {
			t.Fatalf("source %d lacks control BAL", i)
		}
		if i == 1 && missing {
			// An honest but incomplete ETH/72 BAL-serving peer responds with
			// the protocol-defined unavailable marker, not corrupted data.
			for _, block := range blocks {
				rawdb.DeleteAccessList(services[i].ChainDb(), block.Hash(), block.NumberU64())
			}
			if got := services[i].BlockChain().GetAccessListRLP(blocks[0].Hash()); len(got) != 0 {
				t.Fatalf("incomplete peer still serves a BAL after deletion")
			}
		}
	}
	targetDir := filepath.Join(base, "target")
	targetStack, target := openBALNodeAB(t, targetDir, genesis, reconstruct)
	defer func() {
		if targetStack != nil {
			_ = targetStack.Close()
		}
	}()
	if grace {
		target.handler.downloader.ConfigureBALGraceForExperiment(20 * time.Millisecond)
	} else {
		target.handler.downloader.ConfigureBALGraceForExperiment(0)
	}
	// The test supplies a trusted finalized header as in V7. It is not an
	// Engine API forkchoice event; finality ancestry safety is tested by the
	// separate original-source and mutation regressions.
	target.BlockChain().SetFinalized(final.Header())
	for i := range stacks {
		index := i
		balTCPWait(t, "source ready with complete header range", 8*time.Second, func() bool {
			return services[index].handler.blockRange.currentRange().LatestBlock >= final.NumberU64()
		})
		remote := stacks[i].Server().Self()
		if !remote.IP().IsLoopback() || remote.TCP() == 0 {
			t.Fatalf("refuse non-loopback peer: %s", remote.String())
		}
		targetStack.Server().AddPeer(remote)
	}
	balTCPWait(t, "two authenticated ETH TCP peers", 20*time.Second, func() bool {
		return targetStack.Server().PeerCount() == 2 &&
			target.handler.peers.len() == 2 &&
			stacks[0].Server().PeerCount() == 1 &&
			stacks[1].Server().PeerCount() == 1
	})
	minimumETH := uint(1000)
	for _, peer := range target.handler.peers.all() {
		if peer.Version() < minimumETH {
			minimumETH = peer.Version()
		}
	}
	if minimumETH < 71 {
		t.Fatalf("peer does not support ETH BAL exchange: ETH/%d", minimumETH)
	}
	start := time.Now()
	if err := target.handler.downloader.BeaconSync(final.Header(), final.Header()); err != nil {
		t.Fatalf("two-peer BeaconSync failed: %v", err)
	}
	balTCPWait(t, "two-peer real downloader reaches final head", 70*time.Second, func() bool {
		return target.BlockChain().CurrentBlock().Hash() == final.Hash()
	})
	wall := time.Since(start)
	executed, skipped := balWireCheck(t, target.BlockChain(), blocks, contract, txCount)
	if targetStack.Server().PeerCount() != 2 || target.handler.peers.len() != 2 {
		t.Fatalf("peer topology changed before observed head")
	}
	obs := balTwoPeerObservation{
		DelayMS: delay, Scenario: scenario, Trial: trial, Mode: mode,
		PeerCount: 2, MinimumETH: minimumETH, MissingBALPeer: missing,
		Head: final.Hash().Hex(), StateRoot: final.Root().Hex(),
		Counter: uint64(txCount), Executed: executed, Executionless: skipped,
		WallNS: wall.Nanoseconds(),
	}
	if err := targetStack.Close(); err != nil {
		t.Fatal(err)
	}
	targetStack = nil
	obs.GraceChargedMS = float64(target.handler.downloader.BALGraceConsumedForExperiment()) / float64(time.Millisecond)
	for i := range stacks {
		if err := stacks[i].Close(); err != nil {
			t.Fatal(err)
		}
		stacks[i] = nil
	}
	reopenedStack, reopened := openBALNodeAB(t, targetDir, genesis, reconstruct)
	reopenedExec, reopenedSkip := balWireCheck(t, reopened.BlockChain(), blocks, contract, txCount)
	if err := reopenedStack.Close(); err != nil {
		t.Fatal(err)
	}
	obs.ReopenedExec = reopenedExec
	obs.ReopenedSkip = reopenedSkip
	obs.RestartOK = executed == reopenedExec && skipped == reopenedSkip

	if !obs.RestartOK || obs.Counter != 65 || executed+skipped != txCount {
		t.Fatalf("protected source state/receipts/restart mismatch: %+v", obs)
	}
	if !reconstruct && skipped != 0 {
		t.Fatalf("full-execution control skipped a transaction block: %+v", obs)
	}
	if (grace && (obs.GraceChargedMS < 0 || obs.GraceChargedMS > 25.00001)) ||
		(!grace && obs.GraceChargedMS != 0) {
		t.Fatalf("source-pinned global wait budget violated: %+v", obs)
	}
	return obs
}

// A two-peer topology is held constant within each scenario/delay, with
// orders rotated across three trials. One server always has valid BALs;
// the second either has them or explicitly does not. No requirement is made
// that the downloader must use both peers for every message: connection is
// established, but per-peer BAL scheduling remains UNKNOWN.
func TestBALTwoPeerTCPLatencyFallbackV8(t *testing.T) {
	delay, err := strconv.Atoi(os.Getenv("MG_BAL_TCP_NETEM_MS"))
	if err != nil || (delay != 3 && delay != 15) {
		t.Fatalf("expected network qdisc delay 3ms or 15ms: %v", err)
	}
	genesis, blocks, contract, txCount := makeBALNodeABCorpus(t)
	modes := []string{"execute", "reconstruct-immediate", "reconstruct-grace"}
	orders := [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}}
	scenarios := []string{"two-healthy", "one-missing-bal"}
	for _, scenario := range scenarios {
		for trial, order := range orders {
			var arms [3]balTwoPeerObservation
			for _, index := range order {
				obs := runBALTwoPeerArm(t, trial, delay, scenario, modes[index], genesis, blocks, contract, txCount)
				arms[index] = obs
				buf, err := json.Marshal(obs)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("MG_BAL_TCP_V8_ARM %s", buf)
			}
			for _, arm := range arms[1:] {
				if arms[0].Head != arm.Head ||
					arms[0].StateRoot != arm.StateRoot ||
					arms[0].Counter != arm.Counter {
					t.Fatalf("protected future diverged: scenario=%s delay=%d trial=%d", scenario, delay, trial)
				}
			}
			t.Logf("MG_BAL_TCP_V8_PAIR scenario=%s delay=%d trial=%d execute_ns=%d immediate_ns=%d grace_ns=%d immediate_skips=%d grace_skips=%d",
				scenario, delay, trial,
				arms[0].WallNS, arms[1].WallNS, arms[2].WallNS,
				arms[1].Executionless, arms[2].Executionless)
		}
	}
}
