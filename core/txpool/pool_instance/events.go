package txpool_instance

import (
	"execution/common"

	"execution/core/types"
)

// NewTxsEvent is posted when a batch of transactions enter the transaction pool.
type NewTxsEvent struct{ Txs types.Transactions }

// NewMinedBlockEvent is posted when a block has been imported.
type NewMinedBlockEvent struct{ Block *types.Block }

// RemovedLogsEvent is posted when a reorg happens
//type RemovedLogsEvent struct{ Logs []types.Log }

type ChainEvent struct {
	Block *types.Block
	Hash  common.Hash
	// Logs  []types.Log
}

type ChainSideEvent struct {
	Block *types.Block
}

type ChainHeadEvent struct{ Block *types.Block }
