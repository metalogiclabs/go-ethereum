//go:build balexperiment

package catalyst

import (
 "testing"
 "github.com/ethereum/go-ethereum/common"
 "github.com/ethereum/go-ethereum/core/types"
)

// This gate is deliberately narrower than a full remote competing-fork
// Engine integration: it checks the actual V18 admission predicate with
// adversarial fork data, while V13 exercises the authenticated RPC route.
func TestBALV19CrossForkAdmissionPredicate(t *testing.T) {
 root:=&types.Header{Number:common.Big0,Extra:[]byte("common")}
 a:=&types.Header{Number:common.Big1,ParentHash:root.Hash(),Extra:[]byte("A")}
 b:=&types.Header{Number:common.Big1,ParentHash:root.Hash(),Extra:[]byte("B")}
 ah:=&types.Header{Number:common.Big2,ParentHash:a.Hash(),Extra:[]byte("A-head")}
 bh:=&types.Header{Number:common.Big2,ParentHash:b.Hash(),Extra:[]byte("B-head")}
 m:=map[common.Hash]*types.Header{}
 for _,h:=range []*types.Header{root,a,b,ah,bh}{m[h.Hash()]=h}
 get:=func(h common.Hash)*types.Header{return m[h]}
 for _,pair:=range [][2]common.Hash{{ah.Hash(),b.Hash()},{bh.Hash(),a.Hash()}}{
  if balV18ProveFinalizedAncestor(pair[0],pair[1],get){t.Fatal("wrong fork accepted")}
 }
 if !balV18ProveFinalizedAncestor(ah.Hash(),a.Hash(),get){t.Fatal("correct ancestor rejected")}
 delete(m,a.Hash())
 if balV18ProveFinalizedAncestor(ah.Hash(),root.Hash(),get){t.Fatal("missing ancestry accepted")}
 t.Log("MG_BAL_V19_PREDICATE CROSS_FORK_AND_MISSING_ANCESTRY_REJECTED")
}
