package txpool

import (
	"execution/common"
	instance "execution/core/txpool/pool_instance"
	"execution/core/types"
	"math/big"

	"github.com/ethereum/go-ethereum/event"
)

// Transaction is a helper struct to group together a canonical transaction with
// satellite data items that are needed by the pool but are not part of the chain.
type Transaction struct {
	Tx *types.Transaction // Canonical transactio
}

// SubPool represents a specialized transaction pool that lives on its own (e.g.
// blob pool). Since independent of how many specialized pools we have, they do
// need to be updated in lockstep and assemble into one coherent view for block
// production, this interface defines the common methods that allow the primary
// transaction pool to manage the subpools.
type SubPool interface {
	// Filter is a selector used to decide whether a transaction whould be added
	// to this particular subpool.
	Filter(tx *types.Transaction) bool

	// Init sets the base parameters of the subpool, allowing it to load any saved
	// transactions from disk and also permitting internal maintenance routines to
	// start up.
	//
	// These should not be passed as a constructor argument - nor should the pools
	// start by themselves - in order to keep multiple subpools in lockstep with
	// one another.
	Init(gasTip *big.Int, head *types.Header) error

	// Close terminates any background processing threads and releases any held
	// resources.
	Close() error

	// Reset retrieves the current state of the blockchain and ensures the content
	// of the transaction pool is valid with regard to the chain state.
	Reset(oldHead, newHead *types.Header)

	// SetGasTip updates the minimum price required by the subpool for a new
	// transaction, and drops all transactions below this threshold.
	SetGasTip(tip *big.Int)

	// Has returns an indicator whether subpool has a transaction cached with the
	// given hash.
	Has(hash common.Hash) bool

	// Get returns a transaction if it is contained in the pool, or nil otherwise.
	Get(hash common.Hash) *Transaction

	// Add enqueues a batch of transactions into the pool if they are valid. Due
	// to the large transaction churn, add may postpone fully integrating the tx
	// to a later point to batch multiple ones together.
	Add(txs []*Transaction, local bool, sync bool) []error

	// Pending retrieves all currently processable transactions, grouped by origin
	// account and sorted by nonce.
	Pending(enforceTips bool) map[common.Address][]*types.Transaction

	// SubscribeTransactions subscribes to new transaction events.
	SubscribeTransactions(ch chan<- instance.NewTxsEvent) event.Subscription

	// Nonce returns the next nonce of an account, with all transactions executable
	// by the pool already applied on top.
	Nonce(addr common.Address) uint64

	// Stats retrieves the current pool stats, namely the number of pending and the
	// number of queued (non-executable) transactions.
	Stats() (int, int)

	// Content retrieves the data content of the transaction pool, returning all the
	// pending as well as queued transactions, grouped by account and sorted by nonce.
	Content() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction)

	// ContentFrom retrieves the data content of the transaction pool, returning the
	// pending as well as queued transactions of this address, grouped by nonce.
	ContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction)

	// Locals retrieves the accounts currently considered local by the pool.
	Locals() []common.Address

	// Status returns the known status (unknown/pending/queued) of a transaction
	// identified by their hashes.
	Status(hash common.Hash) instance.TxStatus
}
