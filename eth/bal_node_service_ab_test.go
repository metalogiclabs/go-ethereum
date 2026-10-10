// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
// Fork-only research: this is not an upstream patch or a P2P-sync test.
package eth

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
)

type balNodeABObservation struct {
	Round            int    `json:"round"`
	Mode             string `json:"mode"`
	Head             string `json:"head"`
	StateRoot        string `json:"state_root"`
	Counter          uint64 `json:"counter"`
	ReceiptCount     int    `json:"receipt_count"`
	ReopenedReceipts int    `json:"reopened_receipts"`
	ImportWallNS     int64  `json:"import_wall_ns"`
	TotalLifecycleNS int64  `json:"total_lifecycle_ns"`
	DataDirBytes     int64  `json:"datadir_bytes"`
	Blocks           int    `json:"blocks"`
	TxBearingBlocks  int    `json:"tx_bearing_blocks"`
	RestartVerified  bool   `json:"restart_verified"`
}

// One fixed fixture is supplied unchanged to each arm, including its chain,
// skeleton headers and consensus-finalized target.
func makeBALNodeABCorpus(t *testing.T, requested ...int) (*core.Genesis, []*types.Block, common.Address, int) {
	t.Helper()
	count := 260
	if len(requested) != 0 {
		count = requested[0]
	}
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("metalogic-fork-only-node-ab-fixture-v1")))
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	contract := common.Address{0xc0, 0x11, 0xec}
	fork := *params.MergedTestChainConfig
	zero := uint64(0)
	fork.AmsterdamTime = &zero
	fork.BogotaTime = nil
	genesis := &core.Genesis{
		Config:     &fork,
		GasLimit:   30_000_000,
		Difficulty: common.Big0,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Alloc: types.GenesisAlloc{
			from:                             {Balance: new(big.Int).Mul(big.NewInt(100000), big.NewInt(params.Ether))},
			contract:                         {Balance: common.Big0, Code: common.FromHex("0x60005460010160005500")},
			params.BeaconRootsAddress:        {Code: params.BeaconRootsCode},
			params.HistoryStorageAddress:     {Code: params.HistoryStorageCode},
			params.WithdrawalQueueAddress:    {Code: params.WithdrawalQueueCode},
			params.ConsolidationQueueAddress: {Code: params.ConsolidationQueueCode},
			params.BuilderDepositAddress:     {Code: params.BuilderDepositCode},
			params.BuilderExitAddress:        {Code: params.BuilderExitCode},
		},
	}
	engine := beacon.New(ethash.NewFaker())
	txBlocks := 0
	_, blocks, _ := core.GenerateChainWithGenesis(genesis, engine, count, func(i int, g *core.BlockGen) {
		if i%4 != 0 {
			return
		}
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   fork.ChainID,
			Nonce:     g.TxNonce(from),
			To:        &contract,
			Value:     common.Big0,
			Gas:       500000,
			GasFeeCap: big.NewInt(1_000_000_000_000),
			GasTipCap: big.NewInt(1_000_000_000),
		}), g.Signer(), key)
		if err != nil {
			t.Fatal(err)
		}
		g.AddTx(tx)
		txBlocks++
	})
	for _, block := range blocks {
		if block.AccessList() == nil {
			t.Fatalf("block %d has no Amsterdam access list", block.NumberU64())
		}
	}
	if expected := (count + 3) / 4; txBlocks != expected {
		t.Fatalf("generated %d transaction blocks, want %d", txBlocks, expected)
	}
	return genesis, blocks, contract, txBlocks
}

func openBALNodeAB(t *testing.T, dir string, genesis *core.Genesis, reconstruct bool) (*node.Node, *Ethereum) {
	t.Helper()
	stack, err := node.New(&node.Config{
		Name:    "bal-node-ab",
		DataDir: dir,
		IPCPath: "",
		P2P:     p2p.Config{NoDiscovery: true, ListenAddr: "127.0.0.1:0", MaxPeers: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := ethconfig.Defaults
	cfg.Genesis = genesis
	cfg.SyncMode = ethconfig.FullSync
	cfg.StateScheme = rawdb.HashScheme
	cfg.DatabaseCache = 64
	cfg.TrieCleanCache = 32
	cfg.TrieDirtyCache = 0
	cfg.SnapshotCache = 0
	cfg.NoPruning = true
	cfg.TxPool.Journal = ""
	cfg.TxPool.NoLocals = true
	cfg.BlobPool.Datadir = ""
	cfg.BALStateReconstruction = reconstruct
	service, err := New(stack, &cfg)
	if err != nil {
		stack.Close()
		t.Fatal(err)
	}
	if err := stack.Start(); err != nil {
		stack.Close()
		t.Fatal(err)
	}
	return stack, service
}

func balNodeABCheck(t *testing.T, service *Ethereum, blocks []*types.Block, contract common.Address, txBlocks int, reconstruct bool) int {
	t.Helper()
	want := blocks[len(blocks)-1]
	got := service.BlockChain().CurrentBlock()
	if got.Hash() != want.Hash() || got.Root != want.Root() {
		t.Fatalf("head/root mismatch: have %s/%s want %s/%s", got.Hash(), got.Root, want.Hash(), want.Root())
	}
	st, err := service.BlockChain().State()
	if err != nil {
		t.Fatal(err)
	}
	if actual := st.GetState(contract, common.Hash{}).Big().Uint64(); actual != uint64(txBlocks) {
		t.Fatalf("state counter %d, want %d", actual, txBlocks)
	}
	receipts := len(service.BlockChain().GetReceiptsByHash(blocks[0].Hash()))
	expected := 1
	if reconstruct {
		expected = 0
	}
	if receipts != expected {
		t.Fatalf("receipt contract differs: have %d want %d reconstruct=%t", receipts, expected, reconstruct)
	}
	return receipts
}

func balNodeABDiskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var sum int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func balNodeABRun(t *testing.T, round int, reconstruct bool, genesis *core.Genesis, blocks []*types.Block, contract common.Address, txBlocks int) balNodeABObservation {
	t.Helper()
	mode := "execute"
	if reconstruct {
		mode = "reconstruct"
	}
	dir := filepath.Join(t.TempDir(), fmt.Sprintf("%s-%d", mode, round))
	lifecycleStart := time.Now()
	stack, service := openBALNodeAB(t, dir, genesis, reconstruct)
	for _, block := range blocks {
		rawdb.WriteSkeletonHeader(service.ChainDb(), block.Header())
	}
	service.BlockChain().SetFinalized(blocks[len(blocks)-1].Header())
	importStart := time.Now()
	n, err := service.BlockChain().InsertChain(blocks)
	importElapsed := time.Since(importStart)
	if err != nil || n != len(blocks) {
		stack.Close()
		t.Fatalf("%s import %d/%d: %v", mode, n, len(blocks), err)
	}
	receipts := balNodeABCheck(t, service, blocks, contract, txBlocks, reconstruct)
	if err := stack.Close(); err != nil {
		t.Fatal(err)
	}
	// A new Geth service instance opens the same durable database after clean stop.
	reopenedStack, reopened := openBALNodeAB(t, dir, genesis, reconstruct)
	reopenedReceipts := balNodeABCheck(t, reopened, blocks, contract, txBlocks, reconstruct)
	if err := reopenedStack.Close(); err != nil {
		t.Fatal(err)
	}
	return balNodeABObservation{
		Round: round, Mode: mode, Head: blocks[len(blocks)-1].Hash().Hex(),
		StateRoot: blocks[len(blocks)-1].Root().Hex(),
		Counter:   uint64(txBlocks), ReceiptCount: receipts,
		ReopenedReceipts: reopenedReceipts, ImportWallNS: importElapsed.Nanoseconds(),
		TotalLifecycleNS: time.Since(lifecycleStart).Nanoseconds(),
		DataDirBytes:     balNodeABDiskBytes(t, dir), Blocks: len(blocks),
		TxBearingBlocks: txBlocks, RestartVerified: true,
	}
}

// A/B includes real Geth protocol-service construction and disk persistence,
// but excludes remote peers, the downloader and whole-network sync. Receipts
// are intentionally not preserved by reconstruction, so this is not a claim
// of full observational equivalence.
func TestBALNodeServicePairedImportAndRestart(t *testing.T) {
	genesis, blocks, contract, txBlocks := makeBALNodeABCorpus(t)
	for round := 0; round < 2; round++ {
		order := []bool{false, true}
		if round%2 == 1 {
			order = []bool{true, false}
		}
		var results [2]balNodeABObservation
		for _, reconstruct := range order {
			obs := balNodeABRun(t, round, reconstruct, genesis, blocks, contract, txBlocks)
			index := 0
			if reconstruct {
				index = 1
			}
			results[index] = obs
			line, err := json.Marshal(obs)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_NODE_AB_EVIDENCE %s", line)
		}
		if results[0].Head != results[1].Head || results[0].StateRoot != results[1].StateRoot || results[0].Counter != results[1].Counter {
			t.Fatalf("round %d protected head/state divergence: %+v", round, results)
		}
		if results[0].ReceiptCount != 1 || results[1].ReceiptCount != 0 {
			t.Fatalf("round %d lost receipt distinction: %+v", round, results)
		}
		t.Logf("MG_NODE_AB_PAIR round=%d execute_ns=%d reconstruct_ns=%d", round, results[0].ImportWallNS, results[1].ImportWallNS)
	}
}

// TestBALNodeServiceFinalizedContinuation composes the earlier node-service
// restart warrant with a new protected future: a transaction-bearing block
// strictly above finality must be EXECUTED after reopening a service whose
// prior finalized state was reconstructed instead of transaction-executed.
// This is fork-only service-level testing without CL/P2P peers or downloader.
func TestBALNodeServiceFinalizedContinuation(t *testing.T) {
	genesis, blocks, contract, txBlocks := makeBALNodeABCorpus(t, 261)
	history := blocks[:260]
	finalized := history[len(history)-1]
	successor := blocks[260]
	if len(successor.Transactions()) != 1 || txBlocks != 66 {
		t.Fatalf("expected one successor transaction and 66 total, got %d and %d",
			len(successor.Transactions()), txBlocks)
	}
	for _, reconstruct := range []bool{false, true} {
		name := "execute-all"
		if reconstruct {
			name = "reconstruct-finalized"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			stack, service := openBALNodeAB(t, dir, genesis, reconstruct)
			for _, block := range blocks {
				rawdb.WriteSkeletonHeader(service.ChainDb(), block.Header())
			}
			service.BlockChain().SetFinalized(finalized.Header())
			if n, err := service.BlockChain().InsertChain(history); err != nil || n != len(history) {
				stack.Close()
				t.Fatalf("historical import %d/%d: %v", n, len(history), err)
			}
			balNodeABCheck(t, service, history, contract, txBlocks-1, reconstruct)
			if err := stack.Close(); err != nil {
				t.Fatal(err)
			}
			// Simulate a CL re-announcing its finality anchor after restarting
			// the same service with the same persisted on-disk data.
			reopenedStack, reopened := openBALNodeAB(t, dir, genesis, reconstruct)
			defer reopenedStack.Close()
			reopened.BlockChain().SetFinalized(finalized.Header())
			balNodeABCheck(t, reopened, history, contract, txBlocks-1, reconstruct)
			if n, err := reopened.BlockChain().InsertChain([]*types.Block{successor}); err != nil || n != 1 {
				t.Fatalf("unfinalized continuation %d/1: %v", n, err)
			}
			historicalReceipts := balNodeABCheck(t, reopened, blocks, contract, txBlocks, reconstruct)
			if got := len(reopened.BlockChain().GetReceiptsByHash(successor.Hash())); got != 1 {
				t.Fatalf("unfinalized successor receipts %d, must retain 1", got)
			}
			t.Logf("MG_NODE_CONTINUATION mode=%s historical_receipts=%d successor_receipts=1 head=%s root=%s",
				name, historicalReceipts, successor.Hash(), successor.Root())
		})
	}
}
