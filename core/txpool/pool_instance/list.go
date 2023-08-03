// Copyright 2016 The go-ethereum Authors
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

package txpool_instance

import (
	"math"
	"math/big"

	"execution/core/types"
)

// List is a "List" of transactions belonging to an account, sorted by account
// nonce. The same type can be used both for storing contiguous transactions for
// the executable/pending queue; and for storing gapped transactions for the non-
// executable/future queue, with minor behavioral changes.
type List struct {
	strict bool       // Whether nonces are strictly continuous or not
	txs    *SortedMap // Heap indexed sorted hash map of the transactions

	costcap *big.Int // Price of the highest costing transaction (reset only if exceeds balance)
	gascap  uint64   // Gas limit of the highest spending transaction (reset only if exceeds block limit)
}

// newList create a new transaction List for maintaining nonce-indexable fast,
// gapped, sortable transaction Lists.
func NewList(strict bool) *List {
	return &List{
		strict:  strict,
		txs:     NewSortedMap(),
		costcap: new(big.Int),
	}
}

// Contains returns whether the  List contains a transaction
// with the provided nonce.
func (l *List) Contains(nonce uint64) bool {
	return l.txs.Get(nonce) != nil
}

func (l *List) GetCost(nonce uint64) *big.Int {
	return l.txs.GetCost(nonce)
}

// Add tries to insert a new transaction into the List, returning whether the
// transaction was accepted, and if yes, any previous transaction it replaced.
//
// If the new transaction is accepted into the List, the Lists' cost and gas
// thresholds are also potentially updated.
func (l *List) Add(tx *types.Transaction, priceBump uint64) (bool, *types.Transaction) {
	// If there's an older better transaction, abort
	old := l.txs.Get(tx.Nonce)
	if old != nil {
		oldPrice := old.GasPrice.Price
		newPrice := tx.GasPrice.Price
		if oldPrice.Cmp(newPrice) >= 0 {
			return false, nil
		}
		// threshold = oldPrice  * (100 + priceBump) / 100
		a := big.NewInt(100 + int64(priceBump))
		aFee := new(big.Int).Mul(a, oldPrice)
		b := big.NewInt(100)
		thresholdFeeCap := aFee.Div(aFee, b)

		// We have to ensure that both the new fee cap and tip are higher than the
		// old ones as well as checking the percentage threshold to ensure that
		// this is accurate for low (Wei-level) gas price replacements.
		if newPrice.Cmp(thresholdFeeCap) < 0 {
			return false, nil
		}
	}
	// Otherwise overwrite the old transaction with the current one
	// and update the total cost, gas cap and cost cap
	l.txs.Put(tx)
	if cost := tx.Cost(); l.costcap.Cmp(cost) < 0 {
		l.costcap = cost
	}
	if gas := tx.GasLimit; l.gascap < gas {
		l.gascap = gas
	}
	return true, old
}

// Forward removes all transactions from the List with a nonce lower than the
// provided threshold. Every removed transaction is returned for any post-removal
// maintenance.
func (l *List) Forward(threshold uint64) types.Transactions {
	txs := l.txs.Forward(threshold)
	return txs
}

// Filter removes all transactions from the List with a cost or gas limit higher
// than the provided thresholds. Every removed transaction is returned for any
// post-removal maintenance. Strict-mode invalidated transactions are also
// returned.
//
// This method uses the cached costcap and gascap to quickly decide if there's even
// a point in calculating all the costs or if the balance covers all. If the threshold
// is lower than the costgas cap, the caps will be reset to a new high after removing
// the newly invalidated transactions.
func (l *List) Filter(costLimit *big.Int, gasLimit uint64) (types.Transactions, types.Transactions) {
	// If all transactions are below the threshold, short circuit
	if l.costcap.Cmp(costLimit) <= 0 && l.gascap <= gasLimit {
		return nil, nil
	}
	l.costcap = new(big.Int).Set(costLimit) // Lower the caps to the thresholds
	l.gascap = gasLimit

	// Filter out all the transactions above the account's funds
	removed := l.txs.Filter(func(tx *types.Transaction) bool {
		return tx.GasLimit > gasLimit || tx.Cost().Cmp(costLimit) > 0
	})

	if len(removed) == 0 {
		return nil, nil
	}
	var invalids types.Transactions
	// If the List was strict, filter anything above the lowest nonce
	if l.strict {
		lowest := uint64(math.MaxUint64)
		for _, tx := range removed {
			if nonce := tx.Nonce; lowest > nonce {
				lowest = nonce
			}
		}
		// TODO: we can use LastElement() here, may be more efficient
		invalids = l.txs.Filter(func(tx *types.Transaction) bool { return tx.Nonce > lowest })
	}
	// Reset total cost
	return removed, invalids
}

// Cap places a hard limit on the number of items, returning all transactions
// exceeding that limit.
func (l *List) Cap(threshold int) types.Transactions {
	txs := l.txs.Cap(threshold)
	return txs
}

// Remove deletes a transaction from the maintained List, returning whether the
// transaction was found, and also returning any transaction invalidated due to
// the deletion (strict mode only).
func (l *List) Remove(tx *types.Transaction) (bool, types.Transactions) {
	// Remove the transaction from the set
	nonce := tx.Nonce
	if removed := l.txs.Remove(nonce); !removed {
		return false, nil
	}
	// In strict mode, filter out non-executable transactions
	if l.strict {
		// TODO: we can use LastElement() here, may be more efficient
		txs := l.txs.Filter(func(tx *types.Transaction) bool { return tx.Nonce > nonce })
		return true, txs
	}
	return true, nil
}

// Ready retrieves a sequentially increasing List of transactions starting at the
// provided nonce that is ready for processing. The returned transactions will be
// removed from the List.
// The start is the virtual nonce of the account, not the first nonce of it.
// Note, all transactions with nonces lower than start will also be returned to
// prevent getting into and invalid state. This is not something that should ever
// happen but better to be self correcting than failing!
func (l *List) Ready(start uint64, threshold *big.Int) types.Transactions {
	txs := l.txs.Ready(start, threshold)
	return txs
}

// Len returns the length of the transaction List.
func (l *List) Len() int {
	return l.txs.Len()
}

// Empty returns whether the List of transactions is empty or not.
func (l *List) Empty() bool {
	return l.Len() == 0
}

// Flatten creates a nonce-sorted slice of transactions based on the loosely
// sorted internal representation. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
func (l *List) Flatten() types.Transactions {
	return l.txs.Flatten()
}

// LastElement returns the last element of a flattened List, thus, the
// transaction with the highest nonce
func (l *List) LastElement() *types.Transaction {
	return l.txs.LastElement()
}
