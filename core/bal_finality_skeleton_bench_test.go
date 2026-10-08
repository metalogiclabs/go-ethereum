// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// BenchmarkSkeletonFinalityScan isolates the repeated database/RLP/hash cost
// of proving every height in a window by walking to the finalized anchor.
// It is not a full chain-import benchmark or a claim about network sync speed.
func BenchmarkSkeletonFinalityScan(b *testing.B) {
	for _, depth := range []uint64{64, 256, 512} {
		b.Run(fmt.Sprintf("window-%d", depth), func(b *testing.B) {
			db := rawdb.NewMemoryDatabase()
			hashes := make([]common.Hash, depth+1)
			for n := uint64(1); n <= depth; n++ {
				header := &types.Header{
					ParentHash: hashes[n-1],
					Number:     new(big.Int).SetUint64(n),
					Difficulty: common.Big0,
					Time:       n,
				}
				rawdb.WriteSkeletonHeader(db, header)
				hashes[n] = header.Hash()
			}
			finalHash := hashes[depth]
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				for target := uint64(1); target <= depth; target++ {
					want := finalHash
					for height := depth; height > target; height-- {
						header := rawdb.ReadSkeletonHeader(db, height)
						if header == nil || header.Hash() != want {
							b.Fatal("skeleton hash ancestry invalid")
						}
						want = header.ParentHash
					}
					if want != hashes[target] {
						b.Fatal("wrong finalized ancestor")
					}
				}
			}
		})
	}
}
