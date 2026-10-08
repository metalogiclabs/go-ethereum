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
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func balSelfdestructQualificationBlocks(t *testing.T) (*balTestEnv, []*types.Block, []common.Address, common.Address) {
	t.Helper()

	beneficiary := common.HexToAddress("0x00000000000000000000000000000000beefbeef")
	preExisting := common.HexToAddress("0x000000000000000000000000000000005e1f0001")
	withdrawalRecipient := common.HexToAddress("0x00000000000000000000000000000000feed0001")

	// Pre-existing SELFDESTRUCT target. Under EIP-6780 it loses its balance but
	// keeps code and storage because it was not created in the same transaction.
	suicideCode := append([]byte{0x73}, beneficiary.Bytes()...)
	suicideCode = append(suicideCode, 0xff)

	env := newBALTestEnv(types.GenesisAlloc{
		preExisting: {
			Code:    suicideCode,
			Balance: big.NewInt(50),
			Storage: map[common.Hash]common.Hash{
				{}: common.BigToHash(big.NewInt(0x99)),
			},
		},
	})
	engine := beacon.New(ethash.NewFaker())

	// Constructor: SSTORE(0, 0x42), then SELFDESTRUCT to beneficiary.
	// Because destruction occurs in the creation transaction, the created
	// account must be absent from final state despite the intermediate write.
	createDestroy := []byte{0x60, 0x42, 0x60, 0x00, 0x55, 0x73}
	createDestroy = append(createDestroy, beneficiary.Bytes()...)
	createDestroy = append(createDestroy, 0xff)

	_, blocks, _ := GenerateChainWithGenesis(env.gspec, engine, 3, func(i int, b *BlockGen) {
		n := b.TxNonce(env.from)
		switch i {
		case 0:
			b.AddTx(env.tx(n, nil, big.NewInt(100), 1_000_000, 0, createDestroy))
		case 1:
			b.AddTx(env.tx(n, &preExisting, common.Big0, 1_000_000, 0, nil))
		case 2:
			b.AddWithdrawal(&types.Withdrawal{
				Validator: 1,
				Address:   withdrawalRecipient,
				Amount:    7,
			})
		}
	})
	for _, block := range blocks {
		if block.AccessList() == nil {
			t.Fatalf("block #%d carries no access list", block.NumberU64())
		}
	}
	created := crypto.CreateAddress(env.from, blocks[0].Transactions()[0].Nonce())
	watched := []common.Address{
		env.from,
		created,
		preExisting,
		beneficiary,
		withdrawalRecipient,
	}
	return env, blocks, watched, created
}

// TestBALReconstructionSelfdestructDifferential probes the EIP-6780 boundary
// where a BAL-only state transition is least obviously sufficient:
//
//   - a contract writes storage and self-destructs in its creation transaction;
//   - a pre-existing contract self-destructs but must retain code and storage;
//   - a later block mutates state through a withdrawal rather than a transaction.
//
// The reconstructed state must remain identical to canonical execution after
// each block under both supported state schemes.
func TestBALReconstructionSelfdestructDifferential(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			env, blocks, watched, created := balSelfdestructQualificationBlocks(t)

			canonical := newBALQualificationChain(t, env, scheme, false)
			defer canonical.Stop()
			reconstructed := newBALQualificationChain(t, env, scheme, true)
			defer reconstructed.Stop()

			for i, block := range blocks {
				if n, err := canonical.InsertChain([]*types.Block{block}); err != nil {
					t.Fatalf("canonical insert block #%d at index %d: inserted=%d err=%v", block.NumberU64(), i, n, err)
				}
				reconstructed.SetFinalized(block.Header())
				if n, err := reconstructed.InsertChain([]*types.Block{block}); err != nil {
					t.Fatalf("reconstructed insert block #%d at index %d: inserted=%d err=%v", block.NumberU64(), i, n, err)
				}
				assertBALQualificationStateEqual(t, block, canonical, reconstructed, watched)

				want, err := canonical.State()
				if err != nil {
					t.Fatal(err)
				}
				got, err := reconstructed.State()
				if err != nil {
					t.Fatal(err)
				}
				// Same-tx destroyed account: intermediate SSTORE must not leave
				// any reachable account or storage in reconstructed final state.
				if want.Exist(created) || got.Exist(created) {
					t.Fatalf("block #%d create-destroy account survived: canonical=%t reconstructed=%t",
						block.NumberU64(), want.Exist(created), got.Exist(created))
				}
				if want.GetState(created, common.Hash{}) != (common.Hash{}) || got.GetState(created, common.Hash{}) != (common.Hash{}) {
					t.Fatalf("block #%d create-destroy storage survived", block.NumberU64())
				}

				if i >= 1 {
					// Pre-existing SELFDESTRUCT retains code/storage under
					// EIP-6780 but transfers away its balance.
					preExisting := watched[2]
					if want.GetState(preExisting, common.Hash{}) != common.BigToHash(big.NewInt(0x99)) {
						t.Fatalf("canonical pre-existing storage changed after SELFDESTRUCT")
					}
					if got.GetState(preExisting, common.Hash{}) != common.BigToHash(big.NewInt(0x99)) {
						t.Fatalf("reconstructed pre-existing storage changed after SELFDESTRUCT")
					}
					if len(want.GetCode(preExisting)) == 0 || len(got.GetCode(preExisting)) == 0 {
						t.Fatalf("pre-existing SELFDESTRUCT removed code")
					}
					if !want.GetBalance(preExisting).IsZero() || !got.GetBalance(preExisting).IsZero() {
						t.Fatalf("pre-existing SELFDESTRUCT balance not cleared")
					}
				}
			}
		})
	}
}
