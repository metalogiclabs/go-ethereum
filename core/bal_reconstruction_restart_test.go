// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

func newBALQualificationChainOnDB(t *testing.T, env *balTestEnv, db ethdb.Database, scheme string, reconstruct bool) *BlockChain {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StateScheme = scheme
	cfg.BALStateReconstruction = reconstruct

	bc, err := NewBlockChain(
		db,
		env.gspec,
		beacon.New(ethash.NewFaker()),
		cfg,
	)
	if err != nil {
		t.Fatalf("new blockchain on existing db (%s, reconstruct=%t): %v", scheme, reconstruct, err)
	}
	return bc
}

// TestBALReconstructionRestartDifferential strengthens the basic differential
// oracle by forcing the reconstructed chain through a full BlockChain teardown
// and reopen after every imported block.
//
// A state that only agrees while the producing StateDB/trie objects are alive is
// not a durable reconstruction. After every restart, the persisted head and the
// same protected state observations must still match the continuously executing
// reference chain, and the next block must remain importable.
func TestBALReconstructionRestartDifferential(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			env, blocks, watched := balQualificationBlocks(t)
			canonicalDB := rawdb.NewMemoryDatabase()
			reconstructedDB := rawdb.NewMemoryDatabase()

			canonical := newBALQualificationChainOnDB(t, env, canonicalDB, scheme, false)
			defer canonical.Stop()
			reconstructed := newBALQualificationChainOnDB(t, env, reconstructedDB, scheme, true)

			for i, block := range blocks {
				if n, err := canonical.InsertChain([]*types.Block{block}); err != nil {
					reconstructed.Stop()
					t.Fatalf("canonical insert block #%d at index %d: inserted=%d err=%v", block.NumberU64(), i, n, err)
				}
				reconstructed.SetFinalized(block.Header())
				if n, err := reconstructed.InsertChain([]*types.Block{block}); err != nil {
					reconstructed.Stop()
					t.Fatalf("reconstructed insert block #%d at index %d: inserted=%d err=%v", block.NumberU64(), i, n, err)
				}
				assertBALQualificationStateEqual(t, block, canonical, reconstructed, watched)

				// Tear down every in-memory chain/trie object and rebuild it from
				// the same underlying database before making any further claim.
				reconstructed.Stop()
				reconstructed = newBALQualificationChainOnDB(t, env, reconstructedDB, scheme, true)

				head := reconstructed.CurrentBlock()
				if head.Hash() != block.Hash() || head.Root != block.Root() {
					reconstructed.Stop()
					t.Fatalf("restart after block #%d restored head=%s/%s want=%s/%s",
						block.NumberU64(), head.Hash(), head.Root, block.Hash(), block.Root())
				}
				assertBALQualificationStateEqual(t, block, canonical, reconstructed, watched)
				if receipts := reconstructed.GetReceiptsByHash(block.Hash()); len(receipts) != 0 {
					reconstructed.Stop()
					t.Fatalf("restart after block #%d reconstructed receipts=%d want=0", block.NumberU64(), len(receipts))
				}
			}
			reconstructed.Stop()
		})
	}
}
