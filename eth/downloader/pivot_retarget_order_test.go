// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package downloader

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSnapPivotRetargetCommitOrder is a source-level ordering regression
// for issue #35922. The new snap/2 state goroutine must not begin before
// the earlier pivot's queued blocks have been committed (which writes the
// canonical-hash index queried by isPivotReorged).
//
// This specifically checks the cross-goroutine handoff order. Other tests
// exercise receipt-chain imports and snap/2 behavior.
func TestSnapPivotRetargetCommitOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "downloader.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var (
		commit token.Pos
		retarget token.Pos
	)
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "processSnapSyncContent" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != "d" {
				return true
			}
			switch selector.Sel.Name {
			case "commitSnapSyncData":
				if commit.IsValid() {
					t.Fatal("unexpected multiple receipt commit sites")
				}
				commit = call.Pos()
			case "syncState":
				// The initial state sync is before the loop. The later
				// call is the pivot-retarget goroutine we're guarding.
				if call.Pos() > retarget {
					retarget = call.Pos()
				}
			}
			return true
		})
	}
	if !commit.IsValid() || !retarget.IsValid() {
		t.Fatalf("could not locate pivot commit and retarget calls (commit %d, retarget %d)", commit, retarget)
	}
	if commit >= retarget {
		t.Fatalf("snap/2 retarget starts at %s before canonical receipt commit at %s; this can reset legitimate pivot progress (#35922)",
			fset.Position(retarget), fset.Position(commit))
	}
}
