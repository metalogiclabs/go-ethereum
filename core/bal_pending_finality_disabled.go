//go:build !balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Production / normal builds have no pending BAL finality authority.
// The experimental path exists only in explicitly tagged fork tests.
package core

import "github.com/ethereum/go-ethereum/core/types"

func (bc *BlockChain) pendingBALFinality() *types.Header {
	return nil
}
