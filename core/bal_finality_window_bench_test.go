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
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
)

// BenchmarkBALFinalityCheckpointWindow compares actual executionless
// eligibility checks with the old repeated skeleton scan on identical
// synthetic in-memory finalized header windows. This measures membership
// verification only, not disk throughput or block execution.
func BenchmarkBALFinalityCheckpointWindow(b *testing.B) {
	for _, depth := range []uint64{64, 256, 512} {
		b.Run(fmt.Sprintf("window-%d", depth), func(b *testing.B) {
			env := newBALTestEnv(nil)
			cfg := DefaultConfig()
			cfg.BALStateReconstruction = true
			db := rawdb.NewMemoryDatabase()
			bc, err := NewBlockChain(db, env.gspec, beacon.New(ethash.NewFaker()), cfg)
			if err != nil {
				b.Fatal(err)
			}
			defer bc.Stop()
			candidates := make([]*types.Block, depth)
			parent := bc.CurrentBlock().Hash()
			list := new(bal.BlockAccessList)
			for n := uint64(1); n <= depth; n++ {
				header := &types.Header{
					ParentHash: parent,
					Number:     new(big.Int).SetUint64(n),
					Difficulty: common.Big0,
					Time:       n + 1000,
				}
				block := types.NewBlockWithHeader(header).WithAccessListUnsafe(list)
				candidates[n-1] = block
				rawdb.WriteSkeletonHeader(db, header)
				parent = block.Hash()
			}
			final := candidates[depth-1].Header()
			bc.SetFinalized(final)
			b.Run("repeated-scan", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, candidate := range candidates {
						want := final.Hash()
						for height := depth; height > candidate.NumberU64(); height-- {
							header := rawdb.ReadSkeletonHeader(db, height)
							if header == nil || header.Hash() != want {
								b.Fatal("invalid reference ancestry")
							}
							want = header.ParentHash
						}
						if want != candidate.Hash() {
							b.Fatal("wrong reference ancestor")
						}
					}
				}
			})
			b.Run("checkpoint-window", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					bc.balFinalityProof.Lock()
					bc.balFinalityProof.finalHash = common.Hash{}
					bc.balFinalityProof.windowHashes = nil
					bc.balFinalityProof.Unlock()
					for _, candidate := range candidates {
						if !bc.useAccessListReconstruction(candidate, vm.Config{}) {
							b.Fatal("finalized member ineligible")
						}
					}
				}
			})
		})
	}
}
