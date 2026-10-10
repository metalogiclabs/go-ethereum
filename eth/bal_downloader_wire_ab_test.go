// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Metalogic fork-only experiment. No public peer is contacted. ETH/71 packets
// pass through real protocol handlers and Geth downloader on a local MsgPipe.
package eth

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// balWireCounter observes encoded ETH protocol messages without changing,
// duplicating or reordering the payload. Counts are evidence of real wire
// request/response processing, not a claim about real TCP latency.
type balWireCounter struct {
	p2p.MsgReadWriter
	headersRequested atomic.Uint64
	bodiesRequested atomic.Uint64
	balsRequested atomic.Uint64
	balsReturned atomic.Uint64
	bodiesReturned atomic.Uint64
}

func (w *balWireCounter) ReadMsg() (p2p.Msg, error) {
	msg, err := w.MsgReadWriter.ReadMsg()
	if err == nil {
		switch msg.Code {
		case eth.GetBlockHeadersMsg:
			w.headersRequested.Add(1)
		case eth.GetBlockBodiesMsg:
			w.bodiesRequested.Add(1)
		case eth.GetBlockAccessListsMsg:
			w.balsRequested.Add(1)
		}
	}
	return msg, err
}

func (w *balWireCounter) WriteMsg(msg p2p.Msg) error {
	err := w.MsgReadWriter.WriteMsg(msg)
	if err == nil {
		switch msg.Code {
		case eth.BlockAccessListsMsg:
			w.balsReturned.Add(1)
		case eth.BlockBodiesMsg:
			w.bodiesReturned.Add(1)
		}
	}
	return err
}

type balWireABObservation struct {
	Round int `json:"round"`
	Mode string `json:"mode"`
	Head string `json:"head"`
	StateRoot string `json:"state_root"`
	Counter uint64 `json:"counter"`
	Blocks int `json:"blocks"`
	TxBlocks int `json:"tx_blocks"`
	ExecutedTxBlocks int `json:"executed_tx_blocks"`
	ExecutionlessTxBlocks int `json:"executionless_tx_blocks"`
	ReopenedExecutedTxBlocks int `json:"reopened_executed_tx_blocks"`
	ReopenedExecutionlessTxBlocks int `json:"reopened_executionless_tx_blocks"`
	HeaderRequests uint64 `json:"header_requests"`
	BodyRequests uint64 `json:"body_requests"`
	BALRequests uint64 `json:"bal_requests"`
	BALResponses uint64 `json:"bal_responses"`
	BodyResponses uint64 `json:"body_responses"`
	SyncWallNS int64 `json:"sync_wall_ns"`
	RestartVerified bool `json:"restart_verified"`
}

func balWireWait(t *testing.T, description string, wait time.Duration, test func() bool, errs <-chan error) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if test() {
			return
		}
		select {
		case err := <-errs:
			t.Fatalf("%s: ETH peer loop exited before readiness: %v", description, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("timed out: %s", description)
}

func balWireCheck(t *testing.T, chain *core.BlockChain, blocks []*types.Block, contract common.Address, expectedCounter int) (executed, executionless int) {
	t.Helper()
	final := blocks[len(blocks)-1]
	head := chain.CurrentBlock()
	if head.Hash() != final.Hash() || head.Root != final.Root() {
		t.Fatalf("wrong head/root: have %s/%s want %s/%s", head.Hash(), head.Root, final.Hash(), final.Root())
	}
	st, err := chain.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.GetState(contract, common.Hash{}).Big().Uint64(); got != uint64(expectedCounter) {
		t.Fatalf("counter = %d, want %d", got, expectedCounter)
	}
	for i, block := range blocks {
		if i%4 != 0 {
			continue
		}
		receipts := len(chain.GetReceiptsByHash(block.Hash()))
		switch receipts {
		case 0:
			executionless++
		case 1:
			executed++
		default:
			t.Fatalf("block %d has %d receipts, expected 0/1", block.NumberU64(), receipts)
		}
	}
	if executed+executionless != expectedCounter {
		t.Fatalf("receipt partition %d+%d != %d", executed, executionless, expectedCounter)
	}
	return executed, executionless
}

// balWireOneArm uses two fresh, independent Geth services with an actual ETH/71
// peer message stream. The source has the exact pre-generated chain and serves
// headers, bodies and BALs; the target runs its own Geth skeleton downloader,
// import machinery and authenticated eligibility checks.
func balWireOneArm(t *testing.T, round int, reconstruct bool, genesis *core.Genesis, blocks []*types.Block, contract common.Address, txCount int) balWireABObservation {
	t.Helper()
	mode := "execute"
	if reconstruct {
		mode = "reconstruct"
	}
	base := t.TempDir()
	sourceStack, source := openBALNodeAB(t, filepath.Join(base, "source"), genesis, false)
	if n, err := source.BlockChain().InsertChain(blocks); err != nil || n != len(blocks) {
		sourceStack.Close()
		t.Fatalf("source prefill: %d/%d %v", n, len(blocks), err)
	}
	for i, block := range blocks {
		if i%4 == 0 && len(source.BlockChain().GetAccessListRLP(block.Hash())) == 0 {
			t.Fatalf("source block %d missing BAL RLP", block.NumberU64())
		}
	}
	targetDir := filepath.Join(base, "target")
	targetStack, target := openBALNodeAB(t, targetDir, genesis, reconstruct)

	final := blocks[len(blocks)-1]
	// Announce finality before downloading. Every execution-skipped candidate
	// must be an exact ancestor of this consensus-provided header.
	target.BlockChain().SetFinalized(final.Header())

	caps := []p2p.Cap{{Name: "eth", Version: eth.ETH71}}
	targetPipe, sourcePipe := p2p.MsgPipe()
	sourceCounter := &balWireCounter{MsgReadWriter: sourcePipe}
	targetPeer := eth.NewPeer(eth.ETH71, p2p.NewPeer(enode.ID{1}, "source", caps), targetPipe, target.txPool, target.blobTxPool, target.BlockChain().Config())
	sourcePeer := eth.NewPeer(eth.ETH71, p2p.NewPeer(enode.ID{2}, "target", caps), sourceCounter, source.txPool, source.blobTxPool, source.BlockChain().Config())
	peerErrs := make(chan error, 2)
	go func() {
		peerErrs <- target.handler.runEthPeer(targetPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(target.handler), peer)
		})
	}()
	go func() {
		peerErrs <- source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})
	}()

	balWireWait(t, "source peer advertised latest block", 6*time.Second, func() bool {
		r := source.handler.blockRange.currentRange()
		return r.LatestBlock >= final.NumberU64()
	}, peerErrs)
	balWireWait(t, "ETH/71 peer registration", 8*time.Second, func() bool {
		return target.handler.peers.len() > 0 && source.handler.peers.len() > 0
	}, peerErrs)

	start := time.Now()
	if err := target.handler.downloader.BeaconSync(final.Header(), final.Header()); err != nil {
		t.Fatalf("ETH wire BeaconSync: %v", err)
	}
	balWireWait(t, "downloader imported all blocks", 50*time.Second, func() bool {
		return target.BlockChain().CurrentBlock().Hash() == final.Hash()
	}, peerErrs)
	elapsed := time.Since(start)
	executed, executionless := balWireCheck(t, target.BlockChain(), blocks, contract, txCount)
	observation := balWireABObservation{
		Round: round, Mode: mode, Head: final.Hash().Hex(), StateRoot: final.Root().Hex(),
		Counter: uint64(txCount), Blocks: len(blocks), TxBlocks: txCount,
		ExecutedTxBlocks: executed, ExecutionlessTxBlocks: executionless,
		HeaderRequests: sourceCounter.headersRequested.Load(),
		BodyRequests: sourceCounter.bodiesRequested.Load(),
		BALRequests: sourceCounter.balsRequested.Load(),
		BALResponses: sourceCounter.balsReturned.Load(),
		BodyResponses: sourceCounter.bodiesReturned.Load(),
		SyncWallNS: elapsed.Nanoseconds(),
	}
	if observation.HeaderRequests == 0 || observation.BodyRequests == 0 || observation.BALRequests == 0 || observation.BALResponses == 0 {
		t.Fatalf("wire coverage missing: %+v", observation)
	}
	if !reconstruct && executionless != 0 {
		t.Fatalf("ordinary execution unexpectedly omitted receipts: %+v", observation)
	}

	// Disconnect test-only peers, close both nodes, then independently reopen
	// the target's durable database and recheck the complete receipt partition.
	targetPipe.Close()
	sourcePipe.Close()
	targetPeer.Close()
	sourcePeer.Close()
	if err := targetStack.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sourceStack.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStack, reopened := openBALNodeAB(t, targetDir, genesis, reconstruct)
	afterExecuted, afterExecutionless := balWireCheck(t, reopened.BlockChain(), blocks, contract, txCount)
	if err := reopenedStack.Close(); err != nil {
		t.Fatal(err)
	}
	observation.ReopenedExecutedTxBlocks = afterExecuted
	observation.ReopenedExecutionlessTxBlocks = afterExecutionless
	observation.RestartVerified = executed == afterExecuted && executionless == afterExecutionless
	if !observation.RestartVerified {
		t.Fatalf("persisted receipt partition changed: %+v", observation)
	}
	return observation
}

// Two orders distinguish directional evidence from first-arm caching.
// This measures a local in-process ETH MsgPipe + real Geth downloader, not
// public peering, transport/TCP latency, or production whole-node throughput.
func TestBALDownloaderWirePairedImportAndRestart(t *testing.T) {
	genesis, blocks, contract, txCount := makeBALNodeABCorpus(t)
	for round := 0; round < 2; round++ {
		order := []bool{false, true}
		if round == 1 {
			order = []bool{true, false}
		}
		var pair [2]balWireABObservation
		for _, reconstruct := range order {
			result := balWireOneArm(t, round, reconstruct, genesis, blocks, contract, txCount)
			index := 0
			if reconstruct {
				index = 1
			}
			pair[index] = result
			line, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_ETH_WIRE_EVIDENCE %s", line)
		}
		if pair[0].Head != pair[1].Head || pair[0].StateRoot != pair[1].StateRoot || pair[0].Counter != pair[1].Counter {
			t.Fatalf("paired head/root divergence at round %d", round)
		}
		t.Logf("MG_ETH_WIRE_PAIR %s", fmt.Sprintf("round=%d execute_ns=%d reconstruct_ns=%d reconstructed_tx_blocks=%d", round, pair[0].SyncWallNS, pair[1].SyncWallNS, pair[1].ExecutionlessTxBlocks))
	}
}
