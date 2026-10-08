// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package core

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// balFinalityWindowSize bounds the dense active proof. Sparse checkpoints
// amortize the initial finalized-header walk without retaining every hash.
const balFinalityWindowSize uint64 = 256

// finalizedBALWindow is volatile, revocable finality-derived evidence. The
// checkpoints and dense hashes are admitted only after checking every parent
// hash link back to the consensus-finalized anchor. The cache must never be
// mistaken for a consensus authority or persisted across process restarts.
type finalizedBALWindow struct {
	sync.Mutex
	finalHash    common.Hash
	finalNumber  uint64
	provenDownTo uint64
	checkpoints  map[uint64]common.Hash
	windowStart  uint64
	windowHashes []common.Hash
}

// finalizedAncestryMember proves that block belongs to the exact finalized
// chain, not merely to a height below finality. Canonical headers are preferred
// when available. During snap skeleton catch-up, the finalized-to-candidate
// ancestry is scanned once, retaining every 256th authenticated hash, and one
// active 256-block window is materialized for O(1) membership lookup.
//
// For a gap of N blocks, proof work is O(N) on sequential traversal and
// evidence storage is O(N/256 + 256). A fork or missing header fails closed.
func (bc *BlockChain) finalizedAncestryMember(final *types.Header, block *types.Block) bool {
	finalHash, finalNumber := final.Hash(), final.Number.Uint64()
	number, hash := block.NumberU64(), block.Hash()
	if number > finalNumber {
		return false
	}
	if number == finalNumber {
		return hash == finalHash
	}

	proof := &bc.balFinalityProof
	proof.Lock()
	defer proof.Unlock()

	if proof.finalHash != finalHash || proof.finalNumber != finalNumber {
		proof.finalHash = finalHash
		proof.finalNumber = finalNumber
		proof.provenDownTo = finalNumber
		proof.checkpoints = map[uint64]common.Hash{finalNumber: finalHash}
		proof.windowHashes = nil
	}

	currentAnchor := func() bool {
		current := bc.CurrentFinalBlock()
		return current != nil && current.Hash() == finalHash
	}
	// Reuse only exact hashes already authenticated by the finalized anchor.
	start := number - number%balFinalityWindowSize
	if proof.windowHashes != nil && proof.windowStart == start {
		offset := number - start
		return currentAnchor() && offset < uint64(len(proof.windowHashes)) && proof.windowHashes[offset] == hash
	}

	// Canonical-chain queries have a cheaper existing indexed ancestor path.
	gap := finalNumber - number
	maxNonCanonical := gap
	ancestor, height := bc.hc.GetAncestor(finalHash, finalNumber, gap, &maxNonCanonical)
	if height == number && ancestor == hash {
		return currentAnchor()
	}

	getHeader := func(want common.Hash, height uint64) *types.Header {
		header := bc.hc.GetHeader(want, height)
		if header == nil {
			header = rawdb.ReadSkeletonHeader(bc.db, height)
		}
		if header == nil || header.Number.Uint64() != height || header.Hash() != want {
			return nil
		}
		return header
	}

	// Extend the sparse certificate down to the beginning of this window.
	// Only commit checkpoints if the entire extension is hash-consistent.
	if start < proof.provenDownTo {
		want := proof.checkpoints[proof.provenDownTo]
		staged := make(map[uint64]common.Hash)
		for height := proof.provenDownTo; height > start; height-- {
			header := getHeader(want, height)
			if header == nil {
				return false
			}
			want = header.ParentHash
			if (height-1)%balFinalityWindowSize == 0 {
				staged[height-1] = want
			}
		}
		if !currentAnchor() {
			return false
		}
		staged[start] = want
		for height, authenticated := range staged {
			proof.checkpoints[height] = authenticated
		}
		proof.provenDownTo = start
	}

	// The upper endpoint is either an authenticated sparse checkpoint or
	// the exact finality anchor. Materialize just one bounded window.
	upper := finalNumber
	if finalNumber-start > balFinalityWindowSize {
		upper = start + balFinalityWindowSize
	}
	want, ok := proof.checkpoints[upper]
	if !ok {
		return false
	}
	dense := make([]common.Hash, int(upper-start)+1)
	for height := upper; height > start; height-- {
		dense[height-start] = want
		header := getHeader(want, height)
		if header == nil {
			return false
		}
		want = header.ParentHash
	}
	dense[0] = want
	if want != proof.checkpoints[start] || !currentAnchor() {
		return false
	}
	proof.windowStart, proof.windowHashes = start, dense
	offset := number - start
	return offset < uint64(len(dense)) && dense[offset] == hash
}
