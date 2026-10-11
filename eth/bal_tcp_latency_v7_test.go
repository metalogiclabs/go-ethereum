//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only real encrypted TCP decision gate. The GitHub workflow supplies
// OS-level loopback latency independently of the Geth application code.
// This is not a public-network experiment and does not alter upstream Geth.
package eth

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

type balTCPv7Evidence struct {
	NetemDelayMS     int               `json:"netem_delay_ms"`
	Trial            int               `json:"trial"`
	Mode             string            `json:"mode"`
	GraceWindowMS    int64             `json:"grace_window_ms"`
	ChargedGraceMS   float64           `json:"charged_grace_ms"`
	Observation      balTCPObservation `json:"observation"`
}

// Run the same pre-generated chain through genuine local Geth-to-Geth TCP.
// The three arms are full execution, reconstruction without a wait, and
// reconstruction with a capped optional 20ms BAL grace. A separate network
// namespace is not asserted: netem is applied to loopback on the isolated CI
// runner, so every TCP arm receives the same configured impairment.
func TestBALRealTCPLatencyGraceGate(t *testing.T) {
	delayMS, err := strconv.Atoi(os.Getenv("MG_BAL_TCP_NETEM_MS"))
	if err != nil || delayMS < 0 || delayMS > 100 {
		t.Fatalf("set a valid, explicit MG_BAL_TCP_NETEM_MS: %v", err)
	}
	genesis, blocks, contract, txCount := makeBALNodeABCorpus(t)

	modes := []struct {
		name        string
		reconstruct bool
		window      time.Duration
	}{
		{"execute", false, 0},
		{"reconstruct-immediate", true, 0},
		{"reconstruct-grace", true, 20 * time.Millisecond},
	}
	// Rotated orders protect against a universal warm-cache/order advantage.
	orders := [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}}
	previousConfigure := balTCPExperimentConfigure
	previousObserve := balTCPExperimentAfterStop
	defer func() {
		balTCPExperimentConfigure = previousConfigure
		balTCPExperimentAfterStop = previousObserve
	}()

	for trial, order := range orders {
		var rows [3]balTCPv7Evidence
		for _, index := range order {
			mode := modes[index]
			charged := time.Duration(0)
			balTCPExperimentConfigure = func(target *Ethereum) {
				target.handler.downloader.ConfigureBALGraceForExperiment(mode.window)
			}
			balTCPExperimentAfterStop = func(target *Ethereum, _ *balTCPObservation) {
				charged = target.handler.downloader.BALGraceConsumedForExperiment()
			}
			obs := balTCPOneArm(t, trial, mode.reconstruct, genesis, blocks, contract, txCount)
			row := balTCPv7Evidence{
				NetemDelayMS: delayMS, Trial: trial, Mode: mode.name,
				GraceWindowMS: mode.window.Milliseconds(),
				ChargedGraceMS: float64(charged) / float64(time.Millisecond),
				Observation: obs,
			}
			if !obs.RestartVerified || obs.Blocks != 260 || obs.TxBlocks != 65 ||
				obs.Counter != 65 || obs.NegotiatedETH < 71 || obs.LocalSocketPeers != 1 {
				t.Fatalf("protected encrypted TCP test invalid: %+v", row)
			}
			if obs.ExecutedTxBlocks+obs.ExecutionlessTxBlocks != txCount ||
				obs.ReopenedExecutedTxBlocks != obs.ExecutedTxBlocks ||
				obs.ReopenedExecutionlessTxBlocks != obs.ExecutionlessTxBlocks {
				t.Fatalf("receipt partition or restart mismatched: %+v", row)
			}
			if charged < 0 || charged > 25*time.Millisecond {
				t.Fatalf("global research wait budget violated: %+v", row)
			}
			if mode.window == 0 && charged != 0 {
				t.Fatalf("zero-grace arm incorrectly waited: %+v", row)
			}
			if !mode.reconstruct && obs.ExecutionlessTxBlocks != 0 {
				t.Fatalf("ordinary execution was skipped: %+v", row)
			}
			rows[index] = row
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_BAL_TCP_V7_ARM %s", encoded)
		}
		// Receipt omission below finality is intentional; protected future
		// remains head, state root, counter, and durable restart.
		ref := rows[0].Observation
		for _, row := range rows[1:] {
			if ref.Head != row.Observation.Head ||
				ref.StateRoot != row.Observation.StateRoot ||
				ref.Counter != row.Observation.Counter {
				t.Fatalf("trial %d different protected outcome: %+v", trial, rows)
			}
		}
		t.Logf("MG_BAL_TCP_V7_PAIR %s", fmt.Sprintf(
			"delay_ms=%d trial=%d execute_ns=%d immediate_ns=%d grace_ns=%d immediate_skips=%d grace_skips=%d charged_ms=%.3f",
			delayMS, trial, rows[0].Observation.SyncWallNS,
			rows[1].Observation.SyncWallNS, rows[2].Observation.SyncWallNS,
			rows[1].Observation.ExecutionlessTxBlocks,
			rows[2].Observation.ExecutionlessTxBlocks,
			rows[2].ChargedGraceMS,
		))
	}
}
