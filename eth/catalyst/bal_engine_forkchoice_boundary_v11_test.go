//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only research: authenticated Engine API over localhost HTTP/JWT,
// encrypted Geth-to-Geth TCP, persistent EL databases. The RPC caller here is
// an independent client connection driven by a test, NOT a real CL consensus
// implementation. No production policy or upstream code is changed.
package catalyst

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	balEngineV11Blocks = 260
	balEngineV11Txs    = 65
)

type balEngineV11Record struct {
	Case                     string  `json:"case"`
	AuthenticatedHTTP        bool    `json:"authenticated_http"`
	EngineMethod             string  `json:"engine_method"`
	InitialEngineStatus      string  `json:"initial_engine_status"`
	FinalizedOnFirstSync     bool    `json:"finalized_before_sync"`
	FinalizedAfterImport     bool    `json:"finalized_after_import"`
	PeerCount                int     `json:"peer_count"`
	Head                     string  `json:"head"`
	StateRoot                string  `json:"state_root"`
	SenderNonce              uint64  `json:"sender_nonce"`
	TransactionBlocks        int     `json:"transaction_blocks"`
	ExecutedTxBlocks         int     `json:"executed_tx_blocks"`
	ExecutionlessTxBlocks    int     `json:"executionless_tx_blocks"`
	ValidatedBALAttachments  int64   `json:"validated_bal_attachments"`
	GraceChargedMS           float64 `json:"grace_charged_ms"`
	FinalizedAfterRPCReplay  bool    `json:"finalized_after_rpc_replay"`
	PendingWitnessAdmissions int64   `json:"pending_witness_admissions"`
	ReopenedReceiptsMatch    bool    `json:"reopened_receipts_match"`
	ReopenedStateMatch       bool    `json:"reopened_state_match"`
	SyncWallMS               float64 `json:"sync_wall_ms"`
}

func makeBALEngineV11Corpus(t *testing.T) (*core.Genesis, []*types.Block, common.Address, common.Address) {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("metalogic-bal-engine-v11-corpus-fixed")))
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
	consensus := beacon.New(ethash.NewFaker())
	_, blocks, _ := core.GenerateChainWithGenesis(genesis, consensus, balEngineV11Blocks, func(i int, g *core.BlockGen) {
		if i%4 != 0 {
			return
		}
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   fork.ChainID,
			Nonce:     g.TxNonce(from),
			To:        &contract,
			Gas:       500000,
			GasFeeCap: big.NewInt(1_000_000_000_000),
			GasTipCap: big.NewInt(1_000_000_000),
		}), g.Signer(), key)
		if err != nil {
			t.Fatal(err)
		}
		g.AddTx(tx)
	})
	for _, b := range blocks {
		if b.AccessList() == nil {
			t.Fatalf("block %d missing Amsterdam BAL", b.NumberU64())
		}
	}
	return genesis, blocks, from, contract
}

func openBALEngineV11Node(t *testing.T, dir string, genesis *core.Genesis, reconstruct, engineAPI bool, apiFactory ...func(*eth.Ethereum) rpc.API) (*node.Node, *eth.Ethereum, string) {
	t.Helper()
	jwtFile := filepath.Join(dir, "jwt.hex")
	stack, err := node.New(&node.Config{
		Name:      "bal-engine-boundary",
		DataDir:   dir,
		IPCPath:   "",
		AuthAddr:  "127.0.0.1",
		AuthPort:  0,
		JWTSecret: jwtFile,
		P2P:       p2p.Config{NoDiscovery: true, ListenAddr: "127.0.0.1:0", MaxPeers: 10},
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
	cfg.EthDiscoveryURLs = nil
	cfg.SnapDiscoveryURLs = nil
	service, err := eth.New(stack, &cfg)
	if err != nil {
		_ = stack.Close()
		t.Fatal(err)
	}
	if engineAPI {
		if len(apiFactory) > 0 && apiFactory[0] != nil {
			// Research-only Engine RPC wrapper; actual HTTP/JWT auth remains
			// enforced by the same Node authenticated API registration.
			stack.RegisterAPIs([]rpc.API{apiFactory[0](service)})
		} else if err := Register(stack, service); err != nil {
			_ = stack.Close()
			t.Fatal(err)
		}
	}
	if err := stack.Start(); err != nil {
		_ = stack.Close()
		t.Fatal(err)
	}
	return stack, service, jwtFile
}

func awaitBALEngineV11(t *testing.T, what string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func checkBALEngineV11State(t *testing.T, chain *core.BlockChain, blocks []*types.Block, sender, contract common.Address) (int, int) {
	t.Helper()
	final := blocks[len(blocks)-1]
	head := chain.CurrentBlock()
	if head.Hash() != final.Hash() || head.Root != final.Root() {
		t.Fatalf("head/root mismatch: have %v/%v want %v/%v", head.Hash(), head.Root, final.Hash(), final.Root())
	}
	st, err := chain.State()
	if err != nil {
		t.Fatal(err)
	}
	if nonce := st.GetNonce(sender); nonce != balEngineV11Txs {
		t.Fatalf("sender nonce %d, want %d", nonce, balEngineV11Txs)
	}
	if val := st.GetState(contract, common.Hash{}).Big().Uint64(); val != balEngineV11Txs {
		t.Fatalf("storage counter %d, want %d", val, balEngineV11Txs)
	}
	executed, skipped := 0, 0
	for i, block := range blocks {
		if i%4 != 0 {
			continue
		}
		switch len(chain.GetReceiptsByHash(block.Hash())) {
		case 0:
			skipped++
		case 1:
			executed++
		default:
			t.Fatalf("unexpected receipts at block %d", block.NumberU64())
		}
	}
	if executed+skipped != balEngineV11Txs {
		t.Fatalf("receipt partition %d+%d != %d", executed, skipped, balEngineV11Txs)
	}
	return executed, skipped
}

func runBALEngineV11(t *testing.T, scenario string, genesis *core.Genesis, blocks []*types.Block, sender, contract common.Address) balEngineV11Record {
	t.Helper()
	root := t.TempDir()
	final := blocks[len(blocks)-1]
	frozen := blocks[len(blocks)-2]
	var sources [2]*node.Node
	var servers [2]*eth.Ethereum
	var targetStack *node.Node
	defer func() {
		if targetStack != nil {
			_ = targetStack.Close()
		}
		for _, s := range sources {
			if s != nil {
				_ = s.Close()
			}
		}
	}()
	for i := range sources {
		sources[i], servers[i], _ = openBALEngineV11Node(t, filepath.Join(root, fmt.Sprintf("source-%d", i)), genesis, false, false)
		if n, err := servers[i].BlockChain().InsertChain(blocks); err != nil || n != len(blocks) {
			t.Fatalf("source %d import %d/%d: %v", i, n, len(blocks), err)
		}
		if got := servers[i].BlockChain().GetAccessListRLP(blocks[0].Hash()); len(got) == 0 {
			t.Fatalf("source %d missing first authenticated BAL", i)
		}
	}
	targetDir := filepath.Join(root, "target")
	useEngine := scenario != "direct-pretrusted-finality"
	var target *eth.Ethereum
	var jwtFile string
	var witnessAccepted atomic.Int64
	var apiFactory []func(*eth.Ethereum) rpc.API
	if scenario == "engine-pending-finality" {
		apiFactory = append(apiFactory, func(service *eth.Ethereum) rpc.API {
			return rpc.API{
				Namespace: "engine",
				Service: &balV13PendingEngineAPI{
					ConsensusAPI: NewConsensusAPI(service),
					service:      service,
					onAdmit:      func() { witnessAccepted.Add(1) },
				},
				Authenticated: true,
			}
		})
	}
	targetStack, target, jwtFile = openBALEngineV11Node(t, targetDir, genesis, true, useEngine, apiFactory...)
	target.Downloader().ConfigureBALGraceForExperiment(20 * time.Millisecond)
	var balAttachments atomic.Int64
	target.Downloader().ObserveBALPeerOutcomesForExperiment(func(_ string, _ uint64, outcome string, _ time.Duration) {
		if outcome == "attached" {
			balAttachments.Add(1)
		}
	})
	r := balEngineV11Record{
		Case: scenario, PeerCount: 2, Head: final.Hash().Hex(),
		StateRoot: final.Root().Hex(), SenderNonce: balEngineV11Txs,
		TransactionBlocks: balEngineV11Txs,
	}

	if scenario == "direct-pretrusted-finality" || scenario == "engine-preauthorized-finality" {
		// Test-only intervention: install the exact authenticated fixture
		// anchor before invoking either downloader or real Engine RPC. This is
		// NOT a safe general-purpose production finality admission method.
		target.BlockChain().SetFinalized(frozen.Header())
		r.FinalizedOnFirstSync = target.BlockChain().CurrentFinalBlock() != nil
	} else if scenario != "engine-finalized-unknown" && scenario != "engine-head-equals-finalized" && scenario != "engine-pending-finality" {
		t.Fatalf("unsupported test scenario %q", scenario)
	} else if target.BlockChain().CurrentFinalBlock() != nil {
		t.Fatal("Engine API target started with a finalized block")
	}
	for i := range sources {
		idx := i
		awaitBALEngineV11(t, "source block-range announcement", 8*time.Second, func() bool {
			return servers[idx].BlockChain().CurrentBlock().Hash() == final.Hash()
		})
		peer := sources[i].Server().Self()
		if !peer.IP().IsLoopback() || peer.TCP() == 0 {
			t.Fatalf("refuse nonlocal peer: %s", peer.String())
		}
		targetStack.Server().AddPeer(peer)
	}
	awaitBALEngineV11(t, "two authenticated TCP peers", 20*time.Second, func() bool {
		return targetStack.Server().PeerCount() == 2 &&
			sources[0].Server().PeerCount() == 1 &&
			sources[1].Server().PeerCount() == 1
	})

	start := time.Now()
	if !useEngine {
		if err := target.Downloader().BeaconSync(final.Header(), frozen.Header()); err != nil {
			t.Fatalf("direct control BeaconSync: %v", err)
		}
		r.InitialEngineStatus = "NOT_USED_DIRECT_CONTROL"
	} else {
		secret, err := node.ObtainJWTSecret(jwtFile)
		if err != nil || len(secret) != 32 {
			t.Fatalf("invalid Engine JWT file: %v len=%d", err, len(secret))
		}
		// An actual unauthenticated HTTP request must be rejected, even in
		// this isolated test; the Engine namespace is never public.
		reqBody := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"engine_forkchoiceUpdatedV4","params":[],"id":1}`)
		unauthClient := &http.Client{Timeout: 5 * time.Second}
		resp, err := unauthClient.Post(targetStack.HTTPAuthEndpoint(), "application/json", reqBody)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Engine API unexpectedly allowed unauthenticated RPC: HTTP %d", resp.StatusCode)
		}
		r.AuthenticatedHTTP = true
		r.EngineMethod = "engine_forkchoiceUpdatedV4"
		var jwtSecret [32]byte
		copy(jwtSecret[:], secret)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client, err := rpc.DialOptions(ctx, targetStack.HTTPAuthEndpoint(), rpc.WithHTTPAuth(node.NewJWTAuth(jwtSecret)))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		finalHash := frozen.Hash()
		if scenario == "engine-head-equals-finalized" {
			finalHash = final.Hash()
		}
		fc := engine.ForkchoiceStateV1{
			HeadBlockHash:      final.Hash(),
			FinalizedBlockHash: finalHash,
		}
		var result engine.ForkChoiceResponse
		if err := client.CallContext(ctx, &result, "engine_forkchoiceUpdatedV4", fc, nil, nil); err != nil {
			t.Fatalf("authenticated Engine API forkchoice request failed: %v", err)
		}
		r.InitialEngineStatus = result.PayloadStatus.Status
		if r.InitialEngineStatus != engine.SYNCING {
			t.Fatalf("cold unknown-head forkchoice returned unexpected status %q", r.InitialEngineStatus)
		}
	}
	awaitBALEngineV11(t, "Engine/sync-triggered target head", 70*time.Second, func() bool {
		return target.BlockChain().CurrentBlock().Hash() == final.Hash()
	})
	r.SyncWallMS = float64(time.Since(start)) / float64(time.Millisecond)
	r.ExecutedTxBlocks, r.ExecutionlessTxBlocks = checkBALEngineV11State(t, target.BlockChain(), blocks, sender, contract)
	r.ValidatedBALAttachments = balAttachments.Load()
	r.PendingWitnessAdmissions = witnessAccepted.Load()
	r.FinalizedAfterImport = target.BlockChain().CurrentFinalBlock() != nil
	if !useEngine || scenario == "engine-preauthorized-finality" {
		if r.ValidatedBALAttachments == 0 {
			t.Fatal("positive pretrusted-finality control never acquired a validated BAL")
		}
		if r.ExecutionlessTxBlocks == 0 {
			t.Fatal("pretrusted-finality control did not use verified BAL reconstruction")
		}
	}
	if useEngine {
		if scenario == "engine-preauthorized-finality" {
			if !r.FinalizedAfterImport {
				t.Fatalf("early fixture finality was lost before Engine import: %+v", r)
			}
		} else if scenario == "engine-pending-finality" {
			if r.FinalizedAfterImport || r.PendingWitnessAdmissions == 0 ||
				r.ValidatedBALAttachments == 0 || r.ExecutionlessTxBlocks == 0 {
				t.Fatalf("pending witness did not authorize scoped reconstruction without canonical finality: %+v", r)
			}
		} else {
			// Cold Engine FCU supplies a hash, but does not automatically
			// install a finality authorization during asynchronous import.
			if r.FinalizedAfterImport {
				t.Fatalf("early Engine FCU unexpectedly installed canonical finality: %+v", r)
			}
			if r.ExecutionlessTxBlocks != 0 {
				t.Fatalf("Engine RPC skipped execution without finalized ancestry: %+v", r)
			}
			if r.ValidatedBALAttachments == 0 {
				t.Fatalf("missing BAL evidence: cannot separate finality from acquisition: %+v", r)
			}
		}
		// Once the node knows the block, the SAME authenticated Engine API
		// should accept finality, but that does not retroactively avoid work.
		secret, err := node.ObtainJWTSecret(jwtFile)
		if err != nil {
			t.Fatal(err)
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
		targetFinal := frozen.Hash()
		if scenario == "engine-head-equals-finalized" {
			targetFinal = final.Hash()
		}
		fc := engine.ForkchoiceStateV1{
			HeadBlockHash: final.Hash(), SafeBlockHash: final.Hash(), FinalizedBlockHash: targetFinal,
		}
		var result engine.ForkChoiceResponse
		if err := client.CallContext(ctx, &result, "engine_forkchoiceUpdatedV4", fc, nil, nil); err != nil || result.PayloadStatus.Status != engine.VALID {
			t.Fatalf("Engine finality replay rejected: result=%+v err=%v", result, err)
		}
		r.FinalizedAfterRPCReplay = target.BlockChain().CurrentFinalBlock() != nil
		if !r.FinalizedAfterRPCReplay {
			t.Fatal("authenticated known-head Engine replay failed to install finality")
		}
	}
	r.GraceChargedMS = float64(target.Downloader().BALGraceConsumedForExperiment()) / float64(time.Millisecond)
	if r.GraceChargedMS > 25.00001 {
		t.Fatalf("total grace budget exceeded: %+v", r)
	}
	if scenario == "engine-pending-finality" {
		// The pending authority is a volatile research-only capability.
		// Its lifetime ends with the Engine/sync test node, not a GC cycle.
		target.BlockChain().SetExperimentalPendingBALFinality(nil)
	}
	if err := targetStack.Close(); err != nil {
		t.Fatal(err)
	}
	targetStack = nil
	for i := range sources {
		if err := sources[i].Close(); err != nil {
			t.Fatal(err)
		}
		sources[i] = nil
	}
	reopenedStack, reopened, _ := openBALEngineV11Node(t, targetDir, genesis, true, false)
	x, y := checkBALEngineV11State(t, reopened.BlockChain(), blocks, sender, contract)
	r.ReopenedReceiptsMatch = x == r.ExecutedTxBlocks && y == r.ExecutionlessTxBlocks
	r.ReopenedStateMatch = reopened.BlockChain().CurrentBlock().Root == final.Root()
	if err := reopenedStack.Close(); err != nil {
		t.Fatal(err)
	}
	if !r.ReopenedReceiptsMatch || !r.ReopenedStateMatch {
		t.Fatalf("cold restart changed protected outcome: %+v", r)
	}
	return r
}

func TestBALEngineV11AuthenticatedForkchoiceBoundary(t *testing.T) {
	genesis, blocks, sender, contract := makeBALEngineV11Corpus(t)
	cases := []string{"direct-pretrusted-finality", "engine-finalized-unknown", "engine-head-equals-finalized"}
	var records []balEngineV11Record
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			obs := runBALEngineV11(t, scenario, genesis, blocks, sender, contract)
			raw, err := json.Marshal(obs)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_BAL_ENGINE_V11_ARM %s", raw)
			records = append(records, obs)
		})
	}
	if len(records) != len(cases) {
		t.Fatal("not all engine/positive-control arms finished")
	}
	for _, obs := range records {
		if obs.Head != records[0].Head || obs.StateRoot != records[0].StateRoot ||
			obs.SenderNonce != records[0].SenderNonce {
			t.Fatalf("protected future differs: %+v", records)
		}
	}
}

// TestBALEngineV12ControlledPreauthorization holds the Engine API, corpus,
// peers, finality HASH announcement, source, and protected future fixed while
// varying only whether the exact finalized header is already installed in the
// target before the first authenticated unknown-head Engine FCU. The early
// installation is a deliberately non-production test oracle, not a proposed
// implementation of forkchoice safety or a generalizable CL trust rule.
func TestBALEngineV12ControlledPreauthorization(t *testing.T) {
	genesis, blocks, sender, contract := makeBALEngineV11Corpus(t)
	cases := []string{"engine-finalized-unknown", "engine-preauthorized-finality"}
	var rows []balEngineV11Record
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			result := runBALEngineV11(t, scenario, genesis, blocks, sender, contract)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("MG_BAL_ENGINE_V12_ARM %s", raw)
			rows = append(rows, result)
		})
	}
	if len(rows) != 2 {
		t.Fatal("both authenticated FCU arms are required")
	}
	cold, preauthorized := rows[0], rows[1]
	if !cold.AuthenticatedHTTP || !preauthorized.AuthenticatedHTTP ||
		cold.EngineMethod != preauthorized.EngineMethod ||
		cold.InitialEngineStatus != engine.SYNCING || preauthorized.InitialEngineStatus != engine.SYNCING {
		t.Fatalf("Engine RPC authority was not matched: %+v", rows)
	}
	if cold.Head != preauthorized.Head || cold.StateRoot != preauthorized.StateRoot ||
		cold.SenderNonce != preauthorized.SenderNonce {
		t.Fatalf("protected state changed under authorization intervention: %+v", rows)
	}
	if cold.FinalizedOnFirstSync || cold.ExecutionlessTxBlocks != 0 ||
		!preauthorized.FinalizedOnFirstSync || preauthorized.ExecutionlessTxBlocks == 0 {
		t.Fatalf("finality timing failed to separate causal import paths: %+v", rows)
	}
}
