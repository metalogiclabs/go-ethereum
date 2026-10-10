// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// BenchmarkFinalizedBALBlockImport compares full execution and executionless
// finalized BAL reconstruction on the same generated 260-block Amsterdam chain,
// including the same skeleton-header availability and consensus finality.
// Unlike an eligibility-only benchmark, this times real InsertChain state
// processing and persistence. It excludes peer/network/receipt fetching and
// database cold-cache behavior, so it is NOT a full node-sync benchmark.
func BenchmarkFinalizedBALBlockImport(b *testing.B) {
	for _, stride := range []int{32, 1} {
		b.Run(fmt.Sprintf("260-blocks-tx-every-%d", stride), func(b *testing.B) {
			env := newBALTestEnv(types.GenesisAlloc{
				counterAddr: {Code: counterRuntime, Balance: common.Big0},
			})
			engine := beacon.New(ethash.NewFaker())
			txCount := 0
			_, blocks, _ := GenerateChainWithGenesis(env.gspec, engine, 260, func(i int, g *BlockGen) {
				if i%stride == 0 {
					tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
						ChainID: env.cfg.ChainID, Nonce: g.TxNonce(env.from),
						To: &counterAddr, Value: common.Big0, Gas: 500000,
						GasFeeCap: big.NewInt(1_000_000_000_000), GasTipCap: big.NewInt(1_000_000_000),
					}), env.signer, env.key)
					if err != nil {
						b.Fatal(err)
					}
					g.AddTx(tx)
					txCount++
				}
			})
			for _, block := range blocks {
				if block.AccessList() == nil {
					b.Fatalf("missing BAL at height %d", block.NumberU64())
				}
			}
			target := blocks[len(blocks)-1]
			for _, reconstruct := range []bool{false, true} {
				label := "execute"
				if reconstruct {
					label = "reconstruct-finalized"
				}
				b.Run(label, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						config := DefaultConfig()
						config.BALStateReconstruction = reconstruct
						db := rawdb.NewMemoryDatabase()
						bc, err := NewBlockChain(db, env.gspec, engine, config)
						if err != nil {
							b.Fatal(err)
						}
						for _, blk := range blocks {
							rawdb.WriteSkeletonHeader(db, blk.Header())
						}
						bc.SetFinalized(target.Header())
						b.StartTimer()
						n, err := bc.InsertChain(blocks)
						b.StopTimer()
						if err != nil {
							bc.Stop()
							b.Fatalf("insert %d/%d (%s): %v", n, len(blocks), label, err)
						}
						if head := bc.CurrentBlock(); head.Hash() != target.Hash() || head.Root != target.Root() {
							bc.Stop()
							b.Fatalf("wrong head or root under %s", label)
						}
						state, err := bc.State()
						if err != nil {
							bc.Stop()
							b.Fatal(err)
						}
						if got := state.GetState(counterAddr, common.Hash{}).Big().Uint64(); got != uint64(txCount) {
							bc.Stop()
							b.Fatalf("wrong counter under %s: %d, expected %d", label, got, txCount)
						}
						firstReceipts := len(bc.GetReceiptsByHash(blocks[0].Hash()))
						wantReceipts := 1
						if reconstruct {
							wantReceipts = 0
						}
						if firstReceipts != wantReceipts {
							bc.Stop()
							b.Fatalf("unexpected receipt count under %s: %d vs %d", label, firstReceipts, wantReceipts)
						}
						bc.Stop()
					}
				})
			}
		})
	}
}
