package txpool_instance

import (
	"execution/core/types"
	"math/big"
)

type SortedMap struct {
	items map[uint64]*types.Transaction // Hash map storing the transaction data
	tree  *AVLTree                      // AVL tree of nonces of all the stored transactions (non-strict mode)
}

func NewSortedMap() *SortedMap {
	return &SortedMap{
		items: make(map[uint64]*types.Transaction),
		tree:  new(AVLTree),
	}
}

func (m *SortedMap) Get(nonce uint64) *types.Transaction {
	return m.items[nonce]
}

func (m *SortedMap) GetCost(nonce uint64) *big.Int {
	_, cost := m.tree.Search(nonce)
	return cost
}

func (m *SortedMap) Put(tx *types.Transaction) {
	nonce := tx.Nonce
	m.items[nonce] = tx
	m.tree.Add(nonce, tx.Cost())
}

func (m *SortedMap) Forward(threshold uint64) types.Transactions {
	var remove types.Transactions
	for {
		nonce, err := m.tree.Smallest()
		if nonce >= threshold || err != nil {
			break
		}
		tx := m.items[nonce]
		remove = append(remove, tx)
		m.tree.Remove(nonce)
		delete(m.items, nonce)
	}
	return remove
}

func (m *SortedMap) Filter(filter func(*types.Transaction) bool) types.Transactions {
	var remove types.Transactions
	for nonce, tx := range m.items {
		if filter(tx) {
			remove = append(remove, tx)
			delete(m.items, nonce)
			m.tree.Remove(nonce)
		}
	}
	return remove
}

func (m *SortedMap) Cap(threshold int) types.Transactions {
	// Short circuit if the number of items is under the limit
	size := len(m.items)
	if size <= threshold {
		return nil
	}
	var remove types.Transactions
	for size > threshold {
		nonce, err := m.tree.Largest()
		if err != nil {
			break
		}
		remove = append(remove, m.items[nonce])
		delete(m.items, nonce)
		m.tree.Remove(nonce)
		size--
	}
	return remove
}

func (m *SortedMap) Remove(nonce uint64) bool {
	// Short circuit if no transaction is present
	_, ok := m.items[nonce]
	if !ok {
		return false
	}
	delete(m.items, nonce)
	m.tree.Remove(nonce)
	return true
}

// Given the provided start nonce, Ready returns
// transactions that are continous, the varible start is the virtual nonce.
func (m *SortedMap) Ready(start uint64, threshold *big.Int) types.Transactions {
	size := len(m.items)
	if size == 0 {
		return nil
	}
	smallest, err := m.tree.Smallest()
	if smallest > start || err != nil {
		return nil
	}

	var ready types.Transactions
	tx := m.items[smallest]
	total := new(big.Int).Set(tx.Cost())

	for next := smallest; size > 0 && smallest == next && total.Cmp(threshold) <= 0; next++ {
		ready = append(ready, m.items[next])
		m.tree.Remove(smallest)
		delete(m.items, smallest)
		size--

		smallest, err = m.tree.Smallest()
		if err != nil {
			break
		}
		tx = m.items[smallest]
		total = total.Add(total, tx.Cost())
	}

	return ready
}

func (m *SortedMap) Len() int {
	return len(m.items)
}

func (m *SortedMap) Flatten() types.Transactions {
	nodes := m.tree.Flatten()
	cache := make(types.Transactions, 0, len(m.items))
	for _, node := range nodes {
		cache = append(cache, m.items[node.key])
	}
	return cache
}

func (m *SortedMap) LastElement() *types.Transaction {
	last, err := m.tree.Largest()
	if err != nil {
		return nil
	}
	return m.items[last]
}
