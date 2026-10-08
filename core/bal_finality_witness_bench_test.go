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

// BenchmarkBALFinalitySequentialWitness measures the actual reconstruction
// eligibility guard over a synthetic header-only finalized catch-up window.
// Headers are hash-linked, but bodies/state execution are intentionally absent.
// It does not measure real chain sync, disk locality or full reconstruction.
func BenchmarkBALFinalitySequentialWitness(b *testing.B) {
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
			for i := uint64(1); i <= depth; i++ {
				header := &types.Header{
					ParentHash: parent,
					Number:     new(big.Int).SetUint64(i),
					Difficulty: common.Big0,
					Time:       i + 1000,
				}
				block := types.NewBlockWithHeader(header).WithAccessListUnsafe(list)
				candidates[i-1] = block
				rawdb.WriteSkeletonHeader(db, header)
				parent = block.Hash()
			}
			final := candidates[depth-1].Header()
			bc.SetFinalized(final)
			b.Run("repeated-skeleton-scan", func(b *testing.B) {
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
			b.Run("sequential-witness", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Fresh witness each sweep: include the first authenticated
					// traversal, not only steady-state cached membership.
					bc.balFinalityProof.Lock()
					bc.balFinalityProof.valid = false
					bc.balFinalityProof.final = common.Hash{}
					bc.balFinalityProof.Unlock()
					for _, candidate := range candidates {
						if !bc.useAccessListReconstruction(candidate, vm.Config{}) {
							b.Fatal("finalized sequential candidate ineligible")
						}
					}
				}
			})
		})
	}
}
