package types

import (
	"execution/common"
	"execution/core/state"
	"execution/params"
)

// BlockChain defines the minimal set of methods needed to back a tx pool with
// a chain. Exists to allow mocking the live chain out of tests.
type BlockChain interface {
	// Config retrieves the chain's fork configuration.
	Config() *params.ChainConfig

	// CurrentBlock returns the current head of the chain.
	CurrentBlock() *Header

	// GetBlock retrieves a specific block, used during pool resets.
	GetBlock(hash common.Hash, number uint64) *Block

	// StateAt returns a state database for a given root hash (generally the head).
	StateAt(blockHash common.Hash) (state.StateDB, error)
}
