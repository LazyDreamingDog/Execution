package txpool_instance

import (
	"container/heap"
	"execution/common"
	"execution/core/types"
	"math/big"
	"sync"
	"sync/atomic"
	"time"
)

// priceHeap is a heap.Interface implementation over transactions for retrieving
// price-sorted transactions to discard when the pool fills up. If baseFee is set
// then the heap is sorted based on the effective tip based on the given base fee.
// If baseFee is nil then the sorting is based on gasFeeCap.
type priceHeap struct {
	baseFee *big.Int // heap should always be re-sorted after baseFee is changed
	list    []*types.Transaction
}

func (h *priceHeap) Len() int      { return len(h.list) }
func (h *priceHeap) Swap(i, j int) { h.list[i], h.list[j] = h.list[j], h.list[i] }

func (h *priceHeap) Less(i, j int) bool {
	switch h.cmp(h.list[i], h.list[j]) {
	case -1:
		return true
	case 1:
		return false
	default:
		return (h.list[i]).Nonce > (h.list[j]).Nonce
	}
}

func (h *priceHeap) cmp(a, b *types.Transaction) int {
	return a.GasPrice.Price.Cmp(b.GasPrice.Price)
}

func (h *priceHeap) Push(x interface{}) {
	tx := x.(*types.Transaction)
	h.list = append(h.list, tx)
}

func (h *priceHeap) Pop() interface{} {
	old := h.list
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	h.list = old[0 : n-1]
	return x
}

// PricedList is a price-sorted heap to allow operating on transactions pool
// contents in a price-incrementing way. It's built upon the all transactions
// in txpool but only interested in the remote part. It means only remote transactions
// will be considered for tracking, sorting, eviction, etc.
//
// Two heaps are used for sorting: the urgent heap (based on effective tip in the next
// block) and the floating heap (based on gasFeeCap). Always the bigger heap is chosen for
// eviction. Transactions evicted from the urgent heap are first demoted into the floating heap.
// In some cases (during a congestion, when blocks are full) the urgent heap can provide
// better candidates for inclusion while in other cases (at the top of the baseFee peak)
// the floating heap is better. When baseFee is decreasing they behave similarly.
type PricedList struct {
	// Number of stale price points to (re-heap trigger).
	stales atomic.Int64

	all              *Lookup    // Pointer to the map of all transactions
	urgent, floating priceHeap  // Heaps of prices of all the stored **remote** transactions
	reheapMu         sync.Mutex // Mutex asserts that only one routine is reheaping the list
}

const (
	// urgentRatio : floatingRatio is the capacity ratio of the two queues
	urgentRatio   = 4
	floatingRatio = 1
)

// newPricedList creates a new price-sorted transaction heap.
func NewPricedList(all *Lookup) *PricedList {
	return &PricedList{
		all: all,
	}
}

// Put inserts a new transaction into the heap.
func (l *PricedList) Put(tx *types.Transaction, local bool) {
	if local {
		return
	}
	// Insert every new transaction to the urgent heap first; Discard will balance the heaps
	heap.Push(&l.urgent, tx)
}

// Removed notifies the prices transaction list that an old transaction dropped
// from the pool. The list will just keep a counter of stale objects and update
// the heap if a large enough ratio of transactions go stale.
func (l *PricedList) Removed(count int) {
	// Bump the stale counter, but exit if still too low (< 25%)
	stales := l.stales.Add(int64(count))
	if int(stales) <= (len(l.urgent.list)+len(l.floating.list))/4 {
		return
	}
	// Seems we've reached a critical number of stale transactions, reheap
	l.Reheap()
}

// Underpriced checks whether a transaction is cheaper than (or as cheap as) the
// lowest priced (remote) transaction currently being tracked.
func (l *PricedList) Underpriced(tx *types.Transaction) bool {
	// Note: with two queues, being underpriced is defined as being worse than the worst item
	// in all non-empty queues if there is any. If both queues are empty then nothing is underpriced.
	return (l.underpricedFor(&l.urgent, tx) || len(l.urgent.list) == 0) &&
		(l.underpricedFor(&l.floating, tx) || len(l.floating.list) == 0) &&
		(len(l.urgent.list) != 0 || len(l.floating.list) != 0)
}

// underpricedFor checks whether a transaction is cheaper than (or as cheap as) the
// lowest priced (remote) transaction in the given heap.
func (l *PricedList) underpricedFor(h *priceHeap, tx *types.Transaction) bool {
	// Discard stale price points if found at the heap start
	for len(h.list) > 0 {
		head := h.list[0]
		if l.all.GetRemote((head).TxHash) == nil { // Removed or migrated
			l.stales.Add(-1)
			heap.Pop(h)
			continue
		}
		break
	}
	// Check if the transaction is underpriced or not
	if len(h.list) == 0 {
		return false // There is no remote transaction at all.
	}
	// If the remote transaction is even cheaper than the
	// cheapest one tracked locally, reject it.
	return h.cmp(h.list[0], tx) >= 0
}

// Discard finds a number of most underpriced transactions, removes them from the
// priced list and returns them for further removal from the entire pool.
// If noPending is set to true, we will only consider the floating list
//
// Note local transaction won't be considered for eviction.
func (l *PricedList) Discard(slots int, force bool) (types.Transactions, bool) {
	drop := make(types.Transactions, 0, slots) // Remote underpriced transactions to drop
	for slots > 0 {
		if len(l.urgent.list)*floatingRatio > len(l.floating.list)*urgentRatio {
			// Discard stale transactions if found during cleanup
			tx := heap.Pop(&l.urgent).(*types.Transaction)
			if l.all.GetRemote(tx.TxHash) == nil { // Removed or migrated
				l.stales.Add(-1)
				continue
			}
			// Non stale transaction found, move to floating heap
			heap.Push(&l.floating, tx)
		} else {
			if len(l.floating.list) == 0 {
				// Stop if both heaps are empty
				break
			}
			// Discard stale transactions if found during cleanup
			tx := heap.Pop(&l.floating).(*types.Transaction)
			if l.all.GetRemote(tx.TxHash) == nil { // Removed or migrated
				l.stales.Add(-1)
				continue
			}
			// Non stale transaction found, discard it
			drop = append(drop, tx)
			slots -= numSlots(tx)
		}
	}
	// If we still can't make enough room for the new transaction
	if slots > 0 && !force {
		for _, tx := range drop {
			heap.Push(&l.urgent, tx)
		}
		return nil, false
	}
	return drop, true
}

// Reheap forcibly rebuilds the heap based on the current remote transaction set.
func (l *PricedList) Reheap() {
	l.reheapMu.Lock()
	defer l.reheapMu.Unlock()
	start := time.Now()
	l.stales.Store(0)
	l.urgent.list = make([]*types.Transaction, 0, l.all.RemoteCount())
	l.all.Range(func(hash common.Hash, tx *types.Transaction, local bool) bool {
		l.urgent.list = append(l.urgent.list, tx)
		return true
	}, false, true) // Only iterate remotes
	heap.Init(&l.urgent)

	// balance out the two heaps by moving the worse half of transactions into the
	// floating heap
	// Note: Discard would also do this before the first eviction but Reheap can do
	// it more efficiently. Also, Underpriced would work suboptimally the first time
	// if the floating queue was empty.
	floatingCount := len(l.urgent.list) * floatingRatio / (urgentRatio + floatingRatio)
	l.floating.list = make([]*types.Transaction, floatingCount)
	for i := 0; i < floatingCount; i++ {
		l.floating.list[i] = heap.Pop(&l.urgent).(*types.Transaction)
	}
	heap.Init(&l.floating)
	reheapTimer.Update(time.Since(start))
}

// SetBaseFee updates the base fee and triggers a re-heap. Note that Removed is not
// necessary to call right before SetBaseFee when processing a new block.
func (l *PricedList) SetBaseFee(baseFee *big.Int) {
	l.urgent.baseFee = baseFee
	l.Reheap()
}
