//go:build balexperiment

package catalyst

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestBALV18CompetingForkFinalityFailsClosed(t *testing.T) {
	// Explicitly hash-linked header DAG with a common genesis and two
	// divergent branches. Header contents are unique by Extra.
	genesis := &types.Header{Number: common.Big0, Extra: []byte("genesis")}
	a1 := &types.Header{Number: common.Big1, ParentHash: genesis.Hash(), Extra: []byte("A1")}
	b1 := &types.Header{Number: common.Big1, ParentHash: genesis.Hash(), Extra: []byte("B1")}
	a2 := &types.Header{Number: common.Big2, ParentHash: a1.Hash(), Extra: []byte("A2")}
	b2 := &types.Header{Number: common.Big2, ParentHash: b1.Hash(), Extra: []byte("B2")}
	headers := map[common.Hash]*types.Header{}
	for _, h := range []*types.Header{genesis, a1, b1, a2, b2} {
		headers[h.Hash()] = h
	}
	get := func(hash common.Hash) *types.Header { return headers[hash] }
	cases := []struct {
		name  string
		head  common.Hash
		final common.Hash
		allow bool
	}{
		{"same-A-ancestor", a2.Hash(), a1.Hash(), true},
		{"same-B-ancestor", b2.Hash(), b1.Hash(), true},
		{"wrong-B-finalized-for-A", a2.Hash(), b1.Hash(), false},
		{"wrong-A-finalized-for-B", b2.Hash(), a1.Hash(), false},
		{"finalized-ahead", a1.Hash(), a2.Hash(), false},
		{"zero-head", common.Hash{}, a1.Hash(), false},
		{"zero-finalized", a2.Hash(), common.Hash{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := balV18ProveFinalizedAncestor(tc.head, tc.final, get); got != tc.allow {
				t.Fatalf("ancestry decision %v, expected %v", got, tc.allow)
			}
		})
	}
	delete(headers, a1.Hash())
	if balV18ProveFinalizedAncestor(a2.Hash(), genesis.Hash(), get) {
		t.Fatal("missing intermediate ancestry accepted")
	}
	t.Log("MG_BAL_V18_VERDICT CROSS_FORK_FINALITY_REJECTED_EXACT_HASH_ANCESTRY")
}
