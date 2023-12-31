package txpool_instance

import (
	"crypto/ecdsa"
	crand "crypto/rand"
	"errors"
	"execution/common"
	"execution/core/rawdb"
	"execution/core/state"
	"execution/core/types"
	"execution/core/types/gadget"
	"execution/ethdb"
	"execution/params"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"execution/crypto"

	"github.com/ethereum/go-ethereum/event"
)

var (
	// testTxPoolConfig is a transaction pool configuration without stateful disk
	// sideeffects used during testing.
	testTxPoolConfig Config
)

type stateEnv struct {
	currentDB ethdb.Database
	historyDB ethdb.Database
	state     state.StateDB
}

func newStateEnv() *stateEnv {
	db1 := rawdb.NewMemoryDatabase() // currentDB
	db2 := rawdb.NewMemoryDatabase() // historyDB
	sdb, _ := state.New(state.NewDatabase(db1), state.NewHistoryDB(db2))
	return &stateEnv{currentDB: db1, historyDB: db2, state: *sdb}
}

func init() {
	testTxPoolConfig = DefaultConfig
	testTxPoolConfig.Journal = ""
}

type EasyBlockChain struct {
	config        *params.ChainConfig
	gasLimit      atomic.Uint64
	statedb       state.StateDB
	chainHeadFeed *event.Feed
}

func NewEasyBlockChain(config *params.ChainConfig, gasLimit uint64, statedb state.StateDB, chainHeadFeed *event.Feed) *EasyBlockChain {
	bc := &EasyBlockChain{
		config:        config,
		gasLimit:      atomic.Uint64{},
		statedb:       statedb,
		chainHeadFeed: new(event.Feed),
	}
	bc.gasLimit.Store(gasLimit)
	return bc
}

func (bc *EasyBlockChain) Config() *params.ChainConfig {
	return bc.config
}

func (bc *EasyBlockChain) CurrentBlock() *types.Header {
	return types.NewHeader(common.Hash{}, common.Hash{}, new(big.Int), bc.gasLimit.Load())
}

func (bc *EasyBlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return types.NewBlock(bc.CurrentBlock(), nil)
}

func (bc *EasyBlockChain) StateAt(common.Hash) (state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *EasyBlockChain) SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription {
	return bc.chainHeadFeed.Subscribe(ch)
}

func transaction(nonce uint64, gaslimit uint64, key *ecdsa.PrivateKey) *types.Transaction {
	return pricedTransaction(nonce, gaslimit, big.NewInt(1), key)
}

func pricedTransaction(nonce uint64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	gp := gadget.NewGasPrice(gasprice)
	to := common.Address{}
	to.SetBytes([]byte("to"))
	tx := types.NewNormalTransaction(nonce, to, big.NewInt(100), gaslimit, gp, nil, key)
	return tx
}

func pricedDataTransaction(nonce uint64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey, bytes uint64) *types.Transaction {
	data := make([]byte, bytes)
	crand.Read(data)
	gp := gadget.NewGasPrice(gasprice)
	to := common.Address{}
	to.SetBytes([]byte("to"))
	tx := types.NewNormalTransaction(nonce, to, big.NewInt(100), gaslimit, gp, data, key)
	return tx
}

func setupPool() (*LegacyPool, *ecdsa.PrivateKey) {
	return setupPoolWithConfig()
}

func setupPoolWithConfig() (*LegacyPool, *ecdsa.PrivateKey) {
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 10000000, statedb, new(event.Feed))

	key, _ := crypto.GenerateKey()
	pool := New(testTxPoolConfig, blockchain)
	if err := pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock()); err != nil {
		panic(err)
	}
	// wait for the pool to initialize
	<-pool.initDoneCh
	return pool, key
}

// validatePoolInternals checks various consistency invariants within the pool.
func validatePoolInternals(pool *LegacyPool) error {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	// Ensure the total transaction set is consistent with pending + queued
	pending, queued := pool.stats()
	if total := pool.all.Count(); total != pending+queued {
		return fmt.Errorf("total transaction count %d != %d pending + %d queued", total, pending, queued)
	}
	pool.priced.Reheap()
	priced, remote := pool.priced.urgent.Len()+pool.priced.floating.Len(), pool.all.RemoteCount()
	if priced != remote {
		return fmt.Errorf("total priced transaction count %d != %d", priced, remote)
	}
	// Ensure the next nonce to assign is the correct one
	for addr, txs := range pool.pending {
		// Find the last transaction
		var last uint64
		for nonce := range txs.txs.items {
			if last < nonce {
				last = nonce
			}
		}
		if nonce := pool.pendingNonces.Get(addr); nonce != last+1 {
			return fmt.Errorf("pending nonce mismatch: have %v, want %v", nonce, last+1)
		}
		if txs.txs.tree.root.sum.Cmp(big.NewInt(0)) < 0 {
			return fmt.Errorf("totalcost went negative: %v", txs.txs.tree.root.sum)
		}
	}
	return nil
}

// validateEvents checks that the correct number of transaction addition events
// were fired on the pool's event feed.
func validateEvents(events chan NewTxsEvent, count int) error {
	var received []*types.Transaction

	for len(received) < count {
		select {
		case ev := <-events:
			received = append(received, ev.Txs...)
		case <-time.After(time.Second):
			return fmt.Errorf("event #%d not fired", len(received))
		}
	}
	if len(received) > count {
		return fmt.Errorf("more than %d events fired: %v", count, received[count:])
	}
	select {
	case ev := <-events:
		return fmt.Errorf("more than %d events fired: %v", count, ev.Txs)

	case <-time.After(50 * time.Millisecond):
		// This branch should be "default", but it's a data race between goroutines,
		// reading the event channel and pushing into it, so better wait a bit ensuring
		// really nothing gets injected.
	}
	return nil
}

func deriveSender(tx *types.Transaction) (common.Address, error) {
	return tx.Validation.GetFrom(tx.TxHash)
}

type testChain struct {
	*EasyBlockChain
	address common.Address
	trigger *bool
}

// testChain.State() is used multiple times to reset the pending state.
// when simulate is true it will create a state that indicates
// that tx0 and tx1 are included in the chain.
func (c *testChain) State() (state.StateDB, error) {
	// delay "state change" by one. The tx pool fetches the
	// state multiple times and by delaying it a bit we simulate
	// a state change between those fetches.
	stdb := c.statedb
	if *c.trigger {
		c.statedb = newStateEnv().state
		// simulate that the new head block included tx0 and tx1
		c.statedb.SetNonce(c.address, 2)
		c.statedb.SetBalance(c.address, new(big.Int).SetUint64(1e+18))
		*c.trigger = false
	}
	return stdb, nil
}

func TestStateChangeDuringReset(t *testing.T) {
	t.Parallel()

	var (
		key, _  = crypto.GenerateKey()
		statedb = newStateEnv().state
		trigger = false
	)
	tx0 := transaction(0, 100000, key)
	tx1 := transaction(1, 100000, key)
	address := tx0.From
	// setup pool with 2 transaction in it
	statedb.SetBalance(address, new(big.Int).SetUint64(1e+18))
	blockchain := &testChain{NewEasyBlockChain(nil, 1000000000, statedb, new(event.Feed)), address, &trigger}

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	nonce := pool.Nonce(address)
	if nonce != 0 {
		t.Fatalf("Invalid nonce, want 0, got %d", nonce)
	}

	pool.addRemotesSync([]*types.Transaction{tx0, tx1})

	nonce = pool.Nonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}

	// trigger state change in the background
	trigger = true
	<-pool.requestReset(nil, nil)

	nonce = pool.Nonce(address)
	if nonce != 2 {
		t.Fatalf("Invalid nonce, want 2, got %d", nonce)
	}
}

func testAddBalance(pool *LegacyPool, addr common.Address, amount *big.Int) {
	pool.mu.Lock()
	pool.currentState.AddBalance(addr, amount)
	pool.mu.Unlock()
}

func testSetNonce(pool *LegacyPool, addr common.Address, nonce uint64) {
	pool.mu.Lock()
	pool.currentState.SetNonce(addr, nonce)
	pool.mu.Unlock()
}

func TestInvalidTransactions(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)

	// Intrinsic gas too low
	testAddBalance(pool, from, big.NewInt(1))
	if err, want := pool.addRemote(tx), ErrIntrinsicGas; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	// Insufficient funds
	tx = transaction(0, 100000, key)
	if err, want := pool.addRemote(tx), ErrInsufficientFunds; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	testSetNonce(pool, from, 1)
	testAddBalance(pool, from, big.NewInt(0xffffffffffffff))
	tx = transaction(0, 100000, key)
	if err, want := pool.addRemote(tx), ErrNonceTooLow; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	tx = transaction(1, 100000, key)
	pool.gasTip.Store(big.NewInt(1000))
	if err, want := pool.addRemote(tx), ErrUnderpriced; !errors.Is(err, want) {
		t.Errorf("want %v have %v", want, err)
	}

	if err := pool.addLocal(tx); err != nil {
		t.Error("expected", nil, "got", err)
	}
}

func TestQueue(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100, key)
	from, _ := deriveSender(tx)
	testAddBalance(pool, from, big.NewInt(1000))
	<-pool.requestReset(nil, nil)

	pool.enqueueTx(tx.TxHash, tx, false, true)
	<-pool.requestPromoteExecutables(newAccountSet(from))
	if len(pool.pending) != 1 {
		t.Error("expected valid txs to be 1 is", len(pool.pending))
	}

	tx = transaction(1, 100, key)
	from, _ = deriveSender(tx)
	testSetNonce(pool, from, 2)
	pool.enqueueTx(tx.TxHash, tx, false, true)

	<-pool.requestPromoteExecutables(newAccountSet(from))
	if _, ok := pool.pending[from].txs.items[tx.Nonce]; ok {
		t.Error("expected transaction to be in tx pool")
	}
	if len(pool.queue) > 0 {
		t.Error("expected transaction queue to be empty. is", len(pool.queue))
	}
}

func TestQueue2(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx1 := transaction(0, 100, key)
	tx2 := transaction(10, 100, key)
	tx3 := transaction(11, 100, key)
	from, _ := deriveSender(tx1)
	testAddBalance(pool, from, big.NewInt(1000))
	pool.reset(nil, nil)

	pool.enqueueTx(tx1.TxHash, tx1, false, true)
	pool.enqueueTx(tx2.TxHash, tx2, false, true)
	pool.enqueueTx(tx3.TxHash, tx3, false, true)

	pool.promoteExecutables([]common.Address{from})
	if len(pool.pending) != 1 {
		t.Error("expected pending length to be 1, got", len(pool.pending))
	}
	if pool.queue[from].Len() != 2 {
		t.Error("expected len(queue) == 2, got", pool.queue[from].Len())
	}
}

func TestNegativeValue(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()
	gp := gadget.NewGasPrice(big.NewInt(1))
	tx := types.NewNormalTransaction(0, common.Address{}, big.NewInt(-100), 100, gp, nil, key)
	from, _ := deriveSender(tx)
	testAddBalance(pool, from, big.NewInt(1))
	if err := pool.addRemote(tx); err != ErrNegativeValue {
		t.Error("expected", ErrNegativeValue, "got", err)
	}
}

func TestChainFork(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100000, key)
	addr := tx.From
	resetState := func() {
		statedb := newStateEnv().state
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = NewEasyBlockChain(pool.chainconfig, 1000000, statedb, new(event.Feed))
		<-pool.requestReset(nil, nil)
	}
	resetState()

	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	pool.removeTx(tx.TxHash, true)

	// reset the pool's internal state
	resetState()
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
}

func TestDoubleNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100000, key)
	addr := tx.From

	resetState := func() {
		statedb := newStateEnv().state
		statedb.AddBalance(addr, big.NewInt(100000000000000))

		pool.chain = NewEasyBlockChain(pool.chainconfig, 1000000, statedb, new(event.Feed))
		<-pool.requestReset(nil, nil)
	}
	resetState()

	gp1 := gadget.NewGasPrice(big.NewInt(1))
	gp2 := gadget.NewGasPrice(big.NewInt(2))
	tx1 := types.NewNormalTransaction(0, common.Address{}, big.NewInt(100), 100000, gp1, nil, key)
	tx2 := types.NewNormalTransaction(0, common.Address{}, big.NewInt(100), 1000000, gp2, nil, key)
	tx3 := types.NewNormalTransaction(0, common.Address{}, big.NewInt(100), 1000000, gp1, nil, key)

	// Add the first two transaction, ensure higher priced stays only
	if replace, err := pool.add(tx1, false); err != nil || replace {
		t.Errorf("first transaction insert failed (%v) or reported replacement (%v)", err, replace)
	}
	if replace, err := pool.add(tx2, false); err != nil || !replace {
		t.Errorf("second transaction insert failed (%v) or not reported replacement (%v)", err, replace)
	}
	<-pool.requestPromoteExecutables(newAccountSet(addr))
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.TxHash != tx2.TxHash {
		t.Errorf("transaction mismatch: have %x, want %x", tx.TxHash, tx2.TxHash)
	}

	// Add the third transaction and ensure it's not saved (smaller price)
	pool.add(tx3, false)
	<-pool.requestPromoteExecutables(newAccountSet(addr))
	if pool.pending[addr].Len() != 1 {
		t.Error("expected 1 pending transactions, got", pool.pending[addr].Len())
	}
	if tx := pool.pending[addr].txs.items[0]; tx.TxHash != tx2.TxHash {
		t.Errorf("transaction mismatch: have %x, want %x", tx.TxHash, tx2.TxHash)
	}
	// Ensure the total transaction count is correct
	if pool.all.Count() != 1 {
		t.Error("expected 1 total transactions, got", pool.all.Count())
	}
}

func TestMissingNonce(t *testing.T) {
	t.Parallel()

	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(1, 100000, key)
	addr := tx.From
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	if _, err := pool.add(tx, false); err != nil {
		t.Error("didn't expect error", err)
	}
	if len(pool.pending) != 0 {
		t.Error("expected 0 pending transactions, got", len(pool.pending))
	}
	if pool.queue[addr].Len() != 1 {
		t.Error("expected 1 queued transaction, got", pool.queue[addr].Len())
	}
	if pool.all.Count() != 1 {
		t.Error("expected 1 total transactions, got", pool.all.Count())
	}
}

func TestNonceRecovery(t *testing.T) {
	t.Parallel()

	const n = 10
	pool, key := setupPool()
	defer pool.Close()
	tx := transaction(n, 100000, key)
	addr := tx.From

	testSetNonce(pool, addr, n)
	testAddBalance(pool, addr, big.NewInt(100000000000000))
	<-pool.requestReset(nil, nil)

	if err := pool.addRemote(tx); err != nil {
		t.Error(err)
	}
	// simulate some weird re-order of transactions and missing nonce(s)
	testSetNonce(pool, addr, n-1)
	<-pool.requestReset(nil, nil)
	if fn := pool.Nonce(addr); fn != n-1 {
		t.Errorf("expected nonce to be %d, got %d", n-1, fn)
	}
}

func TestDropping(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	tx := transaction(0, 100000, key)
	account := tx.From
	testAddBalance(pool, account, big.NewInt(1000))

	// Add some pending and some queued transactions
	var (
		tx0  = transaction(0, 100, key)
		tx1  = transaction(1, 200, key)
		tx2  = transaction(2, 300, key)
		tx10 = transaction(10, 100, key)
		tx11 = transaction(11, 200, key)
		tx12 = transaction(12, 300, key)
	)
	pool.all.Add(tx0, false)
	pool.priced.Put(tx0, false)
	pool.promoteTx(account, tx0.TxHash, tx0)

	pool.all.Add(tx1, false)
	pool.priced.Put(tx1, false)
	pool.promoteTx(account, tx1.TxHash, tx1)

	pool.all.Add(tx2, false)
	pool.priced.Put(tx2, false)
	pool.promoteTx(account, tx2.TxHash, tx2)

	pool.enqueueTx(tx10.TxHash, tx10, false, true)
	pool.enqueueTx(tx11.TxHash, tx11, false, true)
	pool.enqueueTx(tx12.TxHash, tx12, false, true)

	// Check that pre and post validations leave the pool as is
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if pool.all.Count() != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 6)
	}

	<-pool.requestReset(nil, nil)
	if pool.pending[account].Len() != 3 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pool.pending[account].Len(), 3)
	}
	if pool.queue[account].Len() != 3 {
		t.Errorf("queued transaction mismatch: have %d, want %d", pool.queue[account].Len(), 3)
	}
	if pool.all.Count() != 6 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 6)
	}

	// Reduce the balance of the account, and check that invalidated transactions are dropped
	testAddBalance(pool, account, big.NewInt(-650))
	<-pool.requestReset(nil, nil)

	if _, ok := pool.pending[account].txs.items[tx0.Nonce]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx1.Nonce]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx2.Nonce]; ok {
		t.Errorf("out-of-fund pending transaction present: %v", tx1)
	}
	if _, ok := pool.queue[account].txs.items[tx10.Nonce]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx11.Nonce]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx12.Nonce]; ok {
		t.Errorf("out-of-fund queued transaction present: %v", tx11)
	}
	if pool.all.Count() != 4 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 4)
	}

	// Reduce the block gas limit, check that invalidated transactions are dropped
	pool.chain.(*EasyBlockChain).gasLimit.Store(100)
	<-pool.requestReset(nil, nil)

	if _, ok := pool.pending[account].txs.items[tx0.Nonce]; !ok {
		t.Errorf("funded pending transaction missing: %v", tx0)
	}
	if _, ok := pool.pending[account].txs.items[tx1.Nonce]; ok {
		t.Errorf("over-gased pending transaction present: %v", tx1)
	}
	if _, ok := pool.queue[account].txs.items[tx10.Nonce]; !ok {
		t.Errorf("funded queued transaction missing: %v", tx10)
	}
	if _, ok := pool.queue[account].txs.items[tx11.Nonce]; ok {
		t.Errorf("over-gased queued transaction present: %v", tx11)
	}
	if pool.all.Count() != 2 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 2)
	}
}

func TestPostponing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the postponing with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create two test accounts to produce different gap profiles with
	keys := make([]*ecdsa.PrivateKey, 2)
	accs := make([]common.Address, len(keys))

	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		accs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
		testAddBalance(pool, accs[i], big.NewInt(50100))
	}
	// Add a batch consecutive pending transactions for validation
	txs := []*types.Transaction{}
	for i, key := range keys {
		for j := 0; j < 100; j++ {
			var tx *types.Transaction
			if (i+j)%2 == 0 {
				tx = transaction(uint64(j), 25000, key)
			} else {
				tx = transaction(uint64(j), 50000, key)
			}
			txs = append(txs, tx)
		}
	}
	for i, err := range pool.addRemotesSync(txs) {
		if err != nil {
			t.Fatalf("tx %d: failed to add transactions: %v", i, err)
		}
	}
	// Check that pre and post validations leave the pool as is
	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != 2 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, 4)
	}
	if queue := pool.queue[accs[0]].Len() + pool.queue[accs[1]].Len(); queue != 128 {
		t.Errorf("queued transaction mismatch: have %d, want %d", queue, 128)
	}
	if pool.all.Count() != 130 { // discard 70 = 35 * 2, and 2 are going to pending, 128 to queue
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 130)
	}

	<-pool.requestReset(nil, nil)

	if pending := pool.pending[accs[0]].Len() + pool.pending[accs[1]].Len(); pending != 2 {
		t.Errorf("pending transaction mismatch: have %d, want %d", pending, 4)
	}
	if queue := pool.queue[accs[0]].Len() + pool.queue[accs[1]].Len(); queue != 128 {
		t.Errorf("queued transaction mismatch: have %d, want %d", queue, 128)
	}
	if pool.all.Count() != 130 { // discard 70 = 35 * 2, and 2 are going to pending, 128 to queue
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 130)
	}

	// Reduce the balance of the account, and check that transactions are reorganised
	for _, addr := range accs {
		testAddBalance(pool, addr, big.NewInt(-1))
	}
	<-pool.requestReset(nil, nil)

	// The first account's first transaction remains valid, check that subsequent
	// ones are either filtered out, or queued up for later.
	if _, ok := pool.pending[accs[0]].txs.items[txs[0].Nonce]; !ok {
		t.Errorf("tx %d: valid and funded transaction missing from pending pool: %v", 0, txs[0])
	}
	// the second account's first transaction is invalid.
	if _, ok := pool.queue[accs[1]].txs.items[txs[100].Nonce]; ok {
		t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", 100, txs[100])
	}

	// every promoteExecutable and demoteUnexcutable function will filter pending and queue with Balance
	// there are lots of transactions discarded
	for i, tx := range txs[1:100] {
		if tx.Nonce >= 65 {
			continue
		}
		if (i+1)%2 == 1 { //cost 50100, which is insufficient
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", i+1, tx)
			}
		} else { // cost 25100, which is sufficient
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", i+1, tx)
			}
		}
	}
	// The second account's first transaction got invalid, check that all transactions
	// are either filtered out, or queued up for later.
	if pool.pending[accs[1]] != nil {
		t.Errorf("invalidated account still has pending transactions")
	}

	for i, tx := range txs[101:] {
		if tx.Nonce >= 65 {
			continue
		}
		if (i+101)%2 == 1 { //cost 50100, which is insufficient
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce]; ok {
				t.Errorf("tx %d: out-of-fund transaction present in future queue: %v", i+101, tx)
			}
		} else { // cost 25100, which is sufficient
			if _, ok := pool.queue[accs[0]].txs.items[tx.Nonce]; !ok {
				t.Errorf("tx %d: valid but future transaction missing from future queue: %v", i+101, tx)
			}
		}
	}
	// half in the queue will be dsiacrded because of insufficient balance
	if pool.all.Count() != 65 {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), 65)
	}
}

// Tests that if the transaction pool has both executable and non-executable
// transactions from an origin account, filling the nonce gap moves all queued
// ones into the pending pool.
func TestGapFilling(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, testTxPoolConfig.AccountQueue+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a pending and a queued transaction with a nonce-gap in between
	pool.addRemotesSync([]*types.Transaction{
		transaction(0, 100000, key),
		transaction(2, 100000, key),
	})
	pending, queued := pool.Stats()
	if pending != 1 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 1)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Fill the nonce gap and ensure all transactions become pending
	if err := pool.addRemoteSync(transaction(1, 100000, key)); err != nil {
		t.Fatalf("failed to add gapped transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("gap-filling event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to a single account goes above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
func TestQueueAccountLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(1); i <= testTxPoolConfig.AccountQueue+5; i++ {
		if err := pool.addRemoteSync(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if len(pool.pending) != 0 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, len(pool.pending), 0)
		}
		if i <= testTxPoolConfig.AccountQueue {
			if pool.queue[account].Len() != int(i) {
				t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), i)
			}
		} else {
			if pool.queue[account].Len() != int(testTxPoolConfig.AccountQueue) {
				t.Errorf("tx %d: queue limit mismatch: have %d, want %d", i, pool.queue[account].Len(), testTxPoolConfig.AccountQueue)
			}
		}
	}
	if pool.all.Count() != int(testTxPoolConfig.AccountQueue) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), testTxPoolConfig.AccountQueue)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some threshold, the higher transactions are dropped to prevent DOS attacks.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestQueueGlobalLimiting(t *testing.T) {
	testQueueGlobalLimiting(t, false)
}
func TestQueueGlobalLimitingNoLocals(t *testing.T) {
	testQueueGlobalLimiting(t, true)
}

func testQueueGlobalLimiting(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.GlobalQueue = config.AccountQueue*3 - 1 // reduce the queue limits to shorten test time (-1 to make it non divisible)

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them (last one will be the local)
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	local := keys[len(keys)-1]

	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := make(types.Transactions, 0, 3*config.GlobalQueue)
	for len(txs) < cap(txs) {
		key := keys[rand.Intn(len(keys)-1)] // skip adding transactions with the local account
		addr := crypto.PubkeyToAddress(key.PublicKey)

		txs = append(txs, transaction(nonces[addr]+1, 100000, key))
		nonces[addr]++
	}
	// Import the batch and verify that limits have been enforced
	pool.addRemotesSync(txs)

	queued := 0
	for addr, list := range pool.queue {
		if list.Len() > int(config.AccountQueue) {
			t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.AccountQueue)
		}
		queued += list.Len()
	}
	if queued > int(config.GlobalQueue) {
		t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.GlobalQueue)
	}
	// Generate a batch of transactions from the local account and import them
	txs = txs[:0]
	for i := uint64(0); i < 3*config.GlobalQueue; i++ {
		txs = append(txs, transaction(i+1, 100000, local))
	}
	pool.addLocals(txs)

	// If locals are disabled, the previous eviction algorithm should apply here too
	if nolocals {
		queued := 0
		for addr, list := range pool.queue {
			if list.Len() > int(config.AccountQueue) {
				t.Errorf("addr %x: queued accounts overflown allowance: %d > %d", addr, list.Len(), config.AccountQueue)
			}
			queued += list.Len()
		}
		if queued > int(config.GlobalQueue) {
			t.Fatalf("total transactions overflow allowance: %d > %d", queued, config.GlobalQueue)
		}
	} else {
		// Local exemptions are enabled, make sure the local account owned the queue
		if len(pool.queue) != 1 {
			t.Errorf("multiple accounts in queue: have %v, want %v", len(pool.queue), 1)
		}
		// Also ensure no local transactions are ever dropped, even if above global limits
		if queued := pool.queue[crypto.PubkeyToAddress(local.PublicKey)].Len(); uint64(queued) != 3*config.GlobalQueue {
			t.Fatalf("local account queued transaction count mismatch: have %v, want %v", queued, 3*config.GlobalQueue)
		}
	}
}

// Tests that if an account remains idle for a prolonged amount of time, any
// non-executable transactions queued up are dropped to prevent wasting resources
// on shuffling them around.
//
// This logic should not hold for local transactions, unless the local tracking
// mechanism is disabled.
func TestQueueTimeLimiting(t *testing.T) {
	testQueueTimeLimiting(t, false)
}
func TestQueueTimeLimitingNoLocals(t *testing.T) {
	testQueueTimeLimiting(t, true)
}

func testQueueTimeLimiting(t *testing.T, nolocals bool) {
	// Reduce the eviction interval to a testable amount
	defer func(old time.Duration) { evictionInterval = old }(evictionInterval)
	evictionInterval = time.Millisecond * 100

	// Create the pool to test the non-expiration enforcement
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.Lifetime = time.Second
	config.NoLocals = nolocals

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add the two transactions and ensure they both are queued up
	if err := pool.addLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(1, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Allow the eviction interval to run
	time.Sleep(2 * evictionInterval)

	// Transactions should not be evicted from the queue yet since lifetime duration has not passed
	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Wait a bit for eviction to run and clean up any leftovers, and ensure only the local remains
	time.Sleep(2 * config.Lifetime)

	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// remove current transactions and increase nonce to prepare for a reset and cleanup
	statedb.SetNonce(crypto.PubkeyToAddress(remote.PublicKey), 2)
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 2)
	<-pool.requestReset(nil, nil)

	// make sure queue, pending are cleared
	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// Queue gapped transactions
	if err := pool.addLocal(pricedTransaction(4, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(4, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	time.Sleep(5 * evictionInterval) // A half lifetime pass

	// Queue executable transactions, the life cycle should be restarted.
	if err := pool.addLocal(pricedTransaction(2, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(2, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	time.Sleep(6 * evictionInterval)

	// All gapped transactions shouldn't be kicked out
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	// The whole life time pass after last promotion, kick out stale transactions
	time.Sleep(2 * config.Lifetime)
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that even if the transaction count belonging to a single account goes
// above some threshold, as long as the transactions are executable, they are
// accepted.
func TestPendingLimiting(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000000))

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, testTxPoolConfig.AccountQueue+5)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Keep queuing up transactions and make sure all above a limit are dropped
	for i := uint64(0); i < testTxPoolConfig.AccountQueue+5; i++ {
		if err := pool.addRemoteSync(transaction(i, 100000, key)); err != nil {
			t.Fatalf("tx %d: failed to add transaction: %v", i, err)
		}
		if pool.pending[account].Len() != int(i)+1 {
			t.Errorf("tx %d: pending pool size mismatch: have %d, want %d", i, pool.pending[account].Len(), i+1)
		}
		if len(pool.queue) != 0 {
			t.Errorf("tx %d: queue size mismatch: have %d, want %d", i, pool.queue[account].Len(), 0)
		}
	}
	if pool.all.Count() != int(testTxPoolConfig.AccountQueue+5) {
		t.Errorf("total transaction mismatch: have %d, want %d", pool.all.Count(), testTxPoolConfig.AccountQueue+5)
	}
	if err := validateEvents(events, int(testTxPoolConfig.AccountQueue+5)); err != nil {
		t.Fatalf("event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, the higher transactions are dropped to prevent DOS
// attacks.
func TestPendingGlobalLimiting(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.GlobalSlots = config.AccountSlots * 10

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.GlobalSlots)/len(keys)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.addRemotesSync(txs)

	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	if pending > int(config.GlobalSlots) {
		t.Fatalf("total pending transactions overflow allowance: %d > %d", pending, config.GlobalSlots)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Test the limit on transaction size is enforced correctly.
// This test verifies every transaction having allowed size
// is added to the pool, and longer transactions are rejected.
func TestAllowedTxSize(t *testing.T) {
	t.Parallel()

	// Create a test account and fund it
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000))

	// Compute maximal data size for transactions (lower bound).

	baseSize := transaction(0, 0, key).Size()
	dataSize := (txMaxSize - baseSize) / 2 // due to the serilization problem, we have to compromise here.

	// Try adding a transaction with maximal allowed size
	tx := pricedDataTransaction(0, (*pool.currentHead.Load()).GasLimit(), big.NewInt(1), key, dataSize)
	if err := pool.addRemoteSync(tx); err != nil {
		t.Fatalf("failed to add transaction of size %d, close to maximal: %v", int(tx.Size()), err)
	}
	// Try adding a transaction with random allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(1, (*pool.currentHead.Load()).GasLimit(), big.NewInt(1), key, uint64(rand.Intn(int(dataSize))))); err != nil {
		t.Fatalf("failed to add transaction of random allowed size: %v", err)
	}
	// Try adding a transaction of minimal not allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(2, (*pool.currentHead.Load()).GasLimit(), big.NewInt(1), key, txMaxSize)); err == nil {
		t.Fatalf("expected rejection on slightly oversize transaction")
	}
	// Try adding a transaction of random not allowed size
	if err := pool.addRemoteSync(pricedDataTransaction(2, (*pool.currentHead.Load()).GasLimit(), big.NewInt(1), key, dataSize+1+uint64(rand.Intn(int(10*txMaxSize))))); err == nil {
		t.Fatalf("expected rejection on oversize transaction")
	}
	// Run some sanity checks on the pool internals
	pending, queued := pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if transactions start being capped, transactions are also removed from 'all'
func TestCapClearsFromAll(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.AccountSlots = 2
	config.AccountQueue = 2
	config.GlobalSlots = 8

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, addr, big.NewInt(1000000))

	txs := types.Transactions{}
	for j := 0; j < int(config.GlobalSlots)*2; j++ {
		txs = append(txs, transaction(uint64(j), 100000, key))
	}
	// Import the batch and verify that limits have been enforced
	pool.addRemotes(txs)
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that if the transaction count belonging to multiple accounts go above
// some hard threshold, if they are under the minimum guaranteed slot count then
// the transactions are still kept.
func TestPendingMinimumAllowance(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.GlobalSlots = 1

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(10000000))
	}
	// Generate and queue a batch of transactions
	nonces := make(map[common.Address]uint64)

	txs := types.Transactions{}
	for _, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		for j := 0; j < int(config.AccountSlots)*2; j++ {
			txs = append(txs, transaction(nonces[addr], 100000, key))
			nonces[addr]++
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.addRemotesSync(txs)

	for addr, list := range pool.pending {
		if list.Len() != int(config.AccountSlots) {
			t.Errorf("addr %x: total pending transactions mismatch: have %d, want %d", addr, list.Len(), config.AccountSlots)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that setting the transaction pool gas price to a higher value correctly
// discards everything cheaper than that and moves any gapped transactions back
// from the pending pool to the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestRepricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 4)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(2), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(2), keys[1]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[2]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2]))
	txs = append(txs, pricedTransaction(3, 100000, big.NewInt(2), keys[2]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[3])

	// Import the batch and that both pending and queued transactions match up
	pool.addRemotesSync(txs)
	pool.addLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 7 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 7)
	}
	if queued != 3 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 3)
	}
	if err := validateEvents(events, 7); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Reprice the pool and check that underpriced transactions get dropped
	pool.SetGasTip(big.NewInt(2))

	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 5 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 5)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Check that we can't add the old transactions back
	if err := pool.addRemote(pricedTransaction(1, 100000, big.NewInt(1), keys[0])); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(1), keys[2])); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("adding underpriced queued transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	}
	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// However we can add local underpriced transactions
	tx := pricedTransaction(1, 100000, big.NewInt(1), keys[3])
	if err := pool.addLocal(tx); err != nil {
		t.Fatalf("failed to add underpriced local transaction: %v", err)
	}
	if pending, _ = pool.Stats(); pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("post-reprice local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// And we can fill gaps with properly priced transactions
	if err := pool.addRemote(pricedTransaction(1, 100000, big.NewInt(2), keys[0])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(2), keys[1])); err != nil {
		t.Fatalf("failed to add pending transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(2), keys[2])); err != nil {
		t.Fatalf("failed to add queued transaction: %v", err)
	}
	if err := validateEvents(events, 5); err != nil {
		t.Fatalf("post-reprice event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that setting the transaction pool gas price to a higher value does not
// remove local transactions.
func TestRepricingKeepsLocals(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(100000*1000000))
	}
	// Create transaction (both pending and queued) with a linearly growing gasprice
	for i := uint64(0); i < 500; i++ {
		// Add pending transaction.
		pendingTx := pricedTransaction(i, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.addLocal(pendingTx); err != nil {
			t.Fatal(err)
		}
		// Add queued transaction.
		queuedTx := pricedTransaction(i+501, 100000, big.NewInt(int64(i)), keys[2])
		if err := pool.addLocal(queuedTx); err != nil {
			t.Fatal(err)
		}
	}
	pending, queued := pool.Stats()
	expPending, expQueued := 500, 500
	validate := func() {
		pending, queued = pool.Stats()
		if pending != expPending {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, expPending)
		}
		if queued != expQueued {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, expQueued)
		}

		if err := validatePoolInternals(pool); err != nil {
			t.Fatalf("pool internal state corrupted: %v", err)
		}
	}
	validate()

	// Reprice the pool and check that nothing is dropped
	pool.SetGasTip(big.NewInt(2))
	validate()

	pool.SetGasTip(big.NewInt(2))
	pool.SetGasTip(big.NewInt(4))
	pool.SetGasTip(big.NewInt(8))
	pool.SetGasTip(big.NewInt(100))
	validate()
}

// Tests that when the pool reaches its global transaction limit, underpriced
// transactions are gradually shifted out for more expensive ones and any gapped
// pending transactions are moved into the queue.
//
// Note, local transactions are never allowed to be dropped.
func TestUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.GlobalSlots = 2
	config.GlobalQueue = 2

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0]))
	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(2), keys[0]))

	txs = append(txs, pricedTransaction(1, 100000, big.NewInt(1), keys[1]))

	ltx := pricedTransaction(0, 100000, big.NewInt(1), keys[2])

	// Import the batch and that both pending and queued transactions match up
	pool.addRemotes(txs)
	pool.addLocal(ltx)

	pending, queued := pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 3); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding an underpriced transaction on block limit fails
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(1), keys[1])); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("adding underpriced pending transaction error mismatch: have %v, want %v", err, ErrUnderpriced)
	}
	// Replace a future transaction with a future transaction
	if err := pool.addRemote(pricedTransaction(1, 100000, big.NewInt(2), keys[1])); err != nil { // +K1:1 => -K1:1 => Pend K0:0, K0:1, K2:0; Que K1:1
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	// Ensure that adding high priced transactions drops cheap ones, but not own
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil { // +K1:0 => -K1:1 => Pend K0:0, K0:1, K1:0, K2:0; Que -
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(4), keys[1])); err != nil { // +K1:2 => -K0:0 => Pend K1:0, K2:0; Que K0:1 K1:2
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(3, 100000, big.NewInt(5), keys[1])); err != nil { // +K1:3 => -K0:1 => Pend K1:0, K2:0; Que K1:2 K1:3
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	// Ensure that replacing a pending transaction with a future transaction fails
	if err := pool.addRemote(pricedTransaction(5, 100000, big.NewInt(6), keys[1])); err != ErrFutureReplacePending {
		t.Fatalf("adding future replace transaction error mismatch: have %v, want %v", err, ErrFutureReplacePending)
	}
	pending, queued = pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding local transactions can push out even higher priced ones
	ltx = pricedTransaction(1, 100000, big.NewInt(0), keys[2])
	if err := pool.addLocal(ltx); err != nil {
		t.Fatalf("failed to append underpriced local transaction: %v", err)
	}
	ltx = pricedTransaction(0, 100000, big.NewInt(0), keys[3])
	if err := pool.addLocal(ltx); err != nil {
		t.Fatalf("failed to add new underpriced local transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != 3 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 3)
	}
	if queued != 1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("local event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that more expensive transactions push out cheap ones from the pool, but
// without producing instability by creating gaps that start jumping transactions
// back and forth between queued/pending.
func TestStableUnderpricing(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.GlobalSlots = 128
	config.GlobalQueue = 0

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a number of test accounts and fund them
	keys := make([]*ecdsa.PrivateKey, 2)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(200*1000000))
	}
	// Fill up the entire queue with the same transaction price points
	txs := types.Transactions{}
	for i := uint64(0); i < config.GlobalSlots; i++ {
		txs = append(txs, pricedTransaction(i, 100000, big.NewInt(1), keys[0]))
	}
	pool.addRemotesSync(txs)

	pending, queued := pool.Stats()
	if pending != int(config.GlobalSlots) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.GlobalSlots)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, int(config.GlobalSlots)); err != nil {
		t.Fatalf("original event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Ensure that adding high priced transactions drops a cheap, but doesn't produce a gap
	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(3), keys[1])); err != nil {
		t.Fatalf("failed to add well priced transaction: %v", err)
	}
	pending, queued = pool.Stats()
	if pending != int(config.GlobalSlots) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, config.GlobalSlots)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validateEvents(events, 1); err != nil {
		t.Fatalf("additional event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the pool rejects duplicate transactions.
func TestDeduplication(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a test account to add transactions with
	key, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Create a batch of transactions and add a few of them
	txs := make([]*types.Transaction, 16)
	for i := 0; i < len(txs); i++ {
		txs[i] = pricedTransaction(uint64(i), 100000, big.NewInt(1), key)
	}
	var firsts []*types.Transaction
	for i := 0; i < len(txs); i += 2 {
		firsts = append(firsts, txs[i])
	}
	errs := pool.addRemotesSync(firsts)
	if len(errs) != len(firsts) {
		t.Fatalf("first add mismatching result count: have %d, want %d", len(errs), len(firsts))
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("add %d failed: %v", i, err)
		}
	}
	pending, queued := pool.Stats()
	if pending != 1 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 1)
	}
	if queued != len(txs)/2-1 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, len(txs)/2-1)
	}
	// Try to add all of them now and ensure previous ones error out as knowns
	errs = pool.addRemotesSync(txs)
	if len(errs) != len(txs) {
		t.Fatalf("all add mismatching result count: have %d, want %d", len(errs), len(txs))
	}
	for i, err := range errs {
		if i%2 == 0 && err == nil {
			t.Errorf("add %d succeeded, should have failed as known", i)
		}
		if i%2 == 1 && err != nil {
			t.Errorf("add %d failed: %v", i, err)
		}
	}
	pending, queued = pool.Stats()
	if pending != len(txs) {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, len(txs))
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that the pool rejects replacement transactions that don't meet the minimum
// price bump required.
func TestReplacement(t *testing.T) {
	t.Parallel()

	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Keep track of transaction events to ensure all executables get announced
	events := make(chan NewTxsEvent, 32)
	sub := pool.txFeed.Subscribe(events)
	defer sub.Unsubscribe()

	// Create a test account to add transactions with
	key, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(key.PublicKey), big.NewInt(1000000000))

	// Add pending transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	price := int64(100)
	threshold := (price * (100 + int64(testTxPoolConfig.PriceBump))) / 100

	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap pending transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(0, 100001, big.NewInt(1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original cheap pending transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("cheap replacement event firing failed: %v", err)
	}

	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper pending transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(0, 100001, big.NewInt(threshold-1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original proper pending transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(0, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper pending transaction: %v", err)
	}
	if err := validateEvents(events, 2); err != nil {
		t.Fatalf("proper replacement event firing failed: %v", err)
	}

	// Add queued transactions, ensuring the minimum price bump is enforced for replacement (for ultra low prices too)
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(1), key)); err != nil {
		t.Fatalf("failed to add original cheap queued transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(2, 100001, big.NewInt(1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original cheap queued transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(2), key)); err != nil {
		t.Fatalf("failed to replace original cheap queued transaction: %v", err)
	}

	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(price), key)); err != nil {
		t.Fatalf("failed to add original proper queued transaction: %v", err)
	}
	if err := pool.addRemote(pricedTransaction(2, 100001, big.NewInt(threshold-1), key)); err != ErrReplaceUnderpriced {
		t.Fatalf("original proper queued transaction replacement error mismatch: have %v, want %v", err, ErrReplaceUnderpriced)
	}
	if err := pool.addRemote(pricedTransaction(2, 100000, big.NewInt(threshold), key)); err != nil {
		t.Fatalf("failed to replace original proper queued transaction: %v", err)
	}

	if err := validateEvents(events, 0); err != nil {
		t.Fatalf("queued replacement event firing failed: %v", err)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}

// Tests that local transactions are journaled to disk, but remote transactions
// get discarded between restarts.
func TestJournaling(t *testing.T)         { testJournaling(t, false) }
func TestJournalingNoLocals(t *testing.T) { testJournaling(t, true) }

func testJournaling(t *testing.T, nolocals bool) {
	t.Parallel()

	// Create a temporary file for the journal
	file, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatalf("failed to create temporary journal: %v", err)
	}
	journal := file.Name()
	defer os.Remove(journal)

	// Clean up the temporary file, we only need the path for now
	file.Close()
	os.Remove(journal)

	// Create the original pool to inject transaction into the journal
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.NoLocals = nolocals
	config.Journal = journal
	config.Rejournal = time.Second

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())

	// Create two test accounts to ensure remotes expire but locals do not
	local, _ := crypto.GenerateKey()
	remote, _ := crypto.GenerateKey()

	testAddBalance(pool, crypto.PubkeyToAddress(local.PublicKey), big.NewInt(1000000000))
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000000))

	// Add three local and a remote transactions and ensure they are queued up
	if err := pool.addLocal(pricedTransaction(0, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.addLocal(pricedTransaction(1, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.addLocal(pricedTransaction(2, 100000, big.NewInt(1), local)); err != nil {
		t.Fatalf("failed to add local transaction: %v", err)
	}
	if err := pool.addRemoteSync(pricedTransaction(0, 100000, big.NewInt(1), remote)); err != nil {
		t.Fatalf("failed to add remote transaction: %v", err)
	}
	pending, queued := pool.Stats()
	if pending != 4 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 4)
	}
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Terminate the old pool, bump the local nonce, create a new pool and ensure relevant transaction survive
	pool.Close()
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool = New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())

	pending, queued = pool.Stats()
	if queued != 0 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
	}
	if nolocals {
		if pending != 0 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
		}
	} else {
		if pending != 2 {
			t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Bump the nonce temporarily and ensure the newly invalidated transaction is removed
	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 2)
	<-pool.requestReset(nil, nil)
	time.Sleep(2 * config.Rejournal)
	pool.Close()

	statedb.SetNonce(crypto.PubkeyToAddress(local.PublicKey), 1)
	blockchain = NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))
	pool = New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())

	pending, queued = pool.Stats()
	if pending != 0 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 0)
	}
	if nolocals {
		if queued != 0 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 0)
		}
	} else {
		if queued != 1 {
			t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 1)
		}
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	pool.Close()
}

// TestStatusCheck tests that the pool can correctly retrieve the
// pending status of individual transactions.
func TestStatusCheck(t *testing.T) {
	t.Parallel()

	// Create the pool to test the status retrievals with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create the test accounts to check various transaction statuses with
	keys := make([]*ecdsa.PrivateKey, 3)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
		testAddBalance(pool, crypto.PubkeyToAddress(keys[i].PublicKey), big.NewInt(1000000))
	}
	// Generate and queue a batch of transactions, both pending and queued
	txs := types.Transactions{}

	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[0])) // Pending only
	txs = append(txs, pricedTransaction(0, 100000, big.NewInt(1), keys[1])) // Pending and queued
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[1]))
	txs = append(txs, pricedTransaction(2, 100000, big.NewInt(1), keys[2])) // Queued only

	// Import the transaction and ensure they are correctly added
	pool.addRemotesSync(txs)

	pending, queued := pool.Stats()
	if pending != 2 {
		t.Fatalf("pending transactions mismatched: have %d, want %d", pending, 2)
	}
	if queued != 2 {
		t.Fatalf("queued transactions mismatched: have %d, want %d", queued, 2)
	}
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	// Retrieve the status of each transaction and validate them
	hashes := make([]common.Hash, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.TxHash
	}
	hashes = append(hashes, common.Hash{})
	expect := []TxStatus{TxStatusPending, TxStatusPending, TxStatusQueued, TxStatusQueued, TxStatusUnknown}

	for i := 0; i < len(hashes); i++ {
		if status := pool.Status(hashes[i]); status != expect[i] {
			t.Errorf("transaction %d: status mismatch: have %v, want %v", i, status, expect[i])
		}
	}
}

func BenchmarkPendingDemotion100(b *testing.B)   { benchmarkPendingDemotion(b, 100) }
func BenchmarkPendingDemotion1000(b *testing.B)  { benchmarkPendingDemotion(b, 1000) }
func BenchmarkPendingDemotion10000(b *testing.B) { benchmarkPendingDemotion(b, 10000) }

func benchmarkPendingDemotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(i), 100000, key)
		pool.promoteTx(account, tx.TxHash, tx)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.demoteUnexecutables()
	}
}

// Benchmarks the speed of scheduling the contents of the future queue of the
// transaction pool.
func BenchmarkFuturePromotion100(b *testing.B)   { benchmarkFuturePromotion(b, 100) }
func BenchmarkFuturePromotion1000(b *testing.B)  { benchmarkFuturePromotion(b, 1000) }
func BenchmarkFuturePromotion10000(b *testing.B) { benchmarkFuturePromotion(b, 10000) }

func benchmarkFuturePromotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(1+i), 100000, key)
		pool.enqueueTx(tx.TxHash, tx, false, true)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.promoteExecutables(nil)
	}
}

// Benchmarks the speed of batched transaction insertion.
func BenchmarkBatchInsert100(b *testing.B)   { benchmarkBatchInsert(b, 100, false) }
func BenchmarkBatchInsert1000(b *testing.B)  { benchmarkBatchInsert(b, 1000, false) }
func BenchmarkBatchInsert10000(b *testing.B) { benchmarkBatchInsert(b, 10000, false) }

func BenchmarkBatchLocalInsert100(b *testing.B)   { benchmarkBatchInsert(b, 100, true) }
func BenchmarkBatchLocalInsert1000(b *testing.B)  { benchmarkBatchInsert(b, 1000, true) }
func BenchmarkBatchLocalInsert10000(b *testing.B) { benchmarkBatchInsert(b, 10000, true) }

func benchmarkBatchInsert(b *testing.B, size int, local bool) {
	// Generate a batch of transactions to enqueue into the pool
	pool, key := setupPool()
	defer pool.Close()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000000000000000))

	batches := make([]types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		batches[i] = make(types.Transactions, size)
		for j := 0; j < size; j++ {
			batches[i][j] = transaction(uint64(size*i+j), 100000, key)
		}
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, batch := range batches {
		if local {
			pool.addLocals(batch)
		} else {
			pool.addRemotes(batch)
		}
	}
}

func BenchmarkInsertRemoteWithAllLocals(b *testing.B) {
	// Allocate keys for testing
	key, _ := crypto.GenerateKey()
	account := crypto.PubkeyToAddress(key.PublicKey)

	remoteKey, _ := crypto.GenerateKey()
	remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)

	locals := make([]*types.Transaction, 4096+1024) // Occupy all slots
	for i := 0; i < len(locals); i++ {
		locals[i] = transaction(uint64(i), 100000, key)
	}
	remotes := make([]*types.Transaction, 1000)
	for i := 0; i < len(remotes); i++ {
		remotes[i] = pricedTransaction(uint64(i), 100000, big.NewInt(2), remoteKey) // Higher gasprice
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pool, _ := setupPool()
		testAddBalance(pool, account, big.NewInt(100000000))
		for _, local := range locals {
			pool.addLocal(local)
		}
		b.StartTimer()
		// Assign a high enough balance for testing
		testAddBalance(pool, remoteAddr, big.NewInt(100000000))
		for i := 0; i < len(remotes); i++ {
			pool.addRemotes([]*types.Transaction{remotes[i]})
		}
		pool.Close()
	}
}

// Benchmarks the speed of batch transaction insertion in case of multiple accounts.
func BenchmarkMultiAccountBatchInsert(b *testing.B) {
	// Generate a batch of transactions to enqueue into the pool
	pool, _ := setupPool()
	defer pool.Close()
	b.ReportAllocs()
	batches := make(types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		key, _ := crypto.GenerateKey()
		account := crypto.PubkeyToAddress(key.PublicKey)
		pool.currentState.AddBalance(account, big.NewInt(1000000))
		tx := transaction(uint64(0), 100000, key)
		batches[i] = tx
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, tx := range batches {
		pool.addRemotesSync([]*types.Transaction{tx})
	}
}

func pricedValuedTransaction(nonce uint64, value int64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx := types.NewNormalTransaction(nonce, common.Address{}, big.NewInt(value), gaslimit, &gadget.GasPrice{Price: gasprice}, nil, key)
	return tx
}

func count(t *testing.T, pool *LegacyPool) (pending int, queued int) {
	t.Helper()
	pending, queued = pool.stats()
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
	return pending, queued
}

func fillPool(t testing.TB, pool *LegacyPool) {
	t.Helper()
	// Create a number of test accounts, fund them and make transactions
	executableTxs := types.Transactions{}
	nonExecutableTxs := types.Transactions{}
	for i := 0; i < 384; i++ {
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(10000000000))
		// Add executable ones
		for j := 0; j < int(pool.config.AccountSlots); j++ {
			executableTxs = append(executableTxs, pricedTransaction(uint64(j), 100000, big.NewInt(300), key))
		}
	}
	// Import the batch and verify that limits have been enforced
	pool.addRemotesSync(executableTxs)
	pool.addRemotesSync(nonExecutableTxs)
	pending, queued := pool.Stats()
	slots := pool.all.Slots()
	// sanity-check that the test prerequisites are ok (pending full)
	if have, want := pending, slots; have != want {
		t.Fatalf("have %d, want %d", have, want)
	}
	if have, want := queued, 0; have != want {
		t.Fatalf("have %d, want %d", have, want)
	}

	t.Logf("pool.config: GlobalSlots=%d, GlobalQueue=%d\n", pool.config.GlobalSlots, pool.config.GlobalQueue)
	t.Logf("pending: %d queued: %d, all: %d\n", pending, queued, slots)
}

func TestTransactionFutureAttack(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig
	config.GlobalQueue = 100
	config.GlobalSlots = 100
	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()
	fillPool(t, pool)
	pending, _ := pool.Stats()
	// Now, future transaction attack starts, let's add a bunch of expensive non-executables, and see if the pending-count drops
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(100000000000))
		futureTxs := types.Transactions{}
		for j := 0; j < int(pool.config.GlobalSlots+pool.config.GlobalQueue); j++ {
			futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 100000, big.NewInt(500), key))
		}
		for i := 0; i < 5; i++ {
			pool.addRemotesSync(futureTxs)
			newPending, newQueued := count(t, pool)
			t.Logf("pending: %d queued: %d, all: %d\n", newPending, newQueued, pool.all.Slots())
		}
	}
	newPending, _ := pool.Stats()
	// Pending should not have been touched
	if have, want := newPending, pending; have < want {
		t.Errorf("wrong pending-count, have %d, want %d (GlobalSlots: %d)",
			have, want, pool.config.GlobalSlots)
	}
}

// Tests that if a batch high-priced of non-executables arrive, they do not kick out
// executable transactions
func TestTransactionFuture1559(t *testing.T) {
	t.Parallel()
	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))
	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts, fund them and make transactions
	fillPool(t, pool)
	pending, _ := pool.Stats()

	// Now, future transaction attack starts, let's add a bunch of expensive non-executables, and see if the pending-count drops
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(100000000000))
		futureTxs := types.Transactions{}
		for j := 0; j < int(pool.config.GlobalSlots+pool.config.GlobalQueue); j++ {
			futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 100000, big.NewInt(301), key))
		}
		pool.addRemotesSync(futureTxs)
	}
	newPending, _ := pool.Stats()
	// Pending should not have been touched
	if have, want := newPending, pending; have != want {
		t.Errorf("Wrong pending-count, have %d, want %d (GlobalSlots: %d)",
			have, want, pool.config.GlobalSlots)
	}
}

// Tests that if a batch of balance-overdraft txs arrive, they do not kick out
// executable transactions
func TestTransactionZAttack(t *testing.T) {
	t.Parallel()
	// Create the pool to test the pricing enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))
	pool := New(testTxPoolConfig, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()
	// Create a number of test accounts, fund them and make transactions
	fillPool(t, pool)

	countInvalidPending := func() int {
		t.Helper()
		var ivpendingNum int
		pendingtxs, _ := pool.Content()
		for account, txs := range pendingtxs {
			cur_balance := new(big.Int).Set(pool.currentState.GetBalance(account))
			for _, tx := range txs {
				if cur_balance.Cmp(tx.Value) <= 0 {
					ivpendingNum++
				} else {
					cur_balance.Sub(cur_balance, tx.Value)
				}
			}
		}
		if err := validatePoolInternals(pool); err != nil {
			t.Fatalf("pool internal state corrupted: %v", err)
		}
		return ivpendingNum
	}
	ivPending := countInvalidPending()
	t.Logf("invalid pending: %d\n", ivPending)

	// Now, DETER-Z attack starts, let's add a bunch of expensive non-executables (from N accounts) along with balance-overdraft txs (from one account), and see if the pending-count drops
	for j := 0; j < int(pool.config.GlobalQueue); j++ {
		futureTxs := types.Transactions{}
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(100000000000))
		futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 21000, big.NewInt(500), key))
		pool.addRemotesSync(futureTxs)
	}

	overDraftTxs := types.Transactions{}
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(100000000000))
		for j := 0; j < int(pool.config.GlobalSlots); j++ {
			overDraftTxs = append(overDraftTxs, pricedValuedTransaction(uint64(j), 600000000000, 21000, big.NewInt(500), key))
		}
	}
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)

	newPending, newQueued := count(t, pool)
	newIvPending := countInvalidPending()
	t.Logf("pool.all.Slots(): %d\n", pool.all.Slots())
	t.Logf("pending: %d queued: %d, all: %d\n", newPending, newQueued, pool.all.Slots())
	t.Logf("invalid pending: %d\n", newIvPending)

	// Pending should not have been touched
	if newIvPending != ivPending {
		t.Errorf("Wrong invalid pending-count, have %d, want %d (GlobalSlots: %d, queued: %d)",
			newIvPending, ivPending, pool.config.GlobalSlots, newQueued)
	}
}

func BenchmarkFutureAttack(b *testing.B) {
	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig
	config.GlobalQueue = 100
	config.GlobalSlots = 100
	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()
	fillPool(b, pool)

	key, _ := crypto.GenerateKey()
	pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), big.NewInt(100000000000))
	futureTxs := types.Transactions{}

	for n := 0; n < b.N; n++ {
		futureTxs = append(futureTxs, pricedTransaction(1000+uint64(n), 100000, big.NewInt(500), key))
	}
	b.ResetTimer()
	for i := 0; i < 5; i++ {
		pool.addRemotesSync(futureTxs)
	}
}

func TestMuteTransaction(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb := newStateEnv().state
	blockchain := NewEasyBlockChain(nil, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig

	pool := New(config, blockchain)
	pool.Init(new(big.Int).SetUint64(config.PriceLimit), blockchain.CurrentBlock())
	defer pool.Close()

	// Create a number of test accounts and fund them
	remote, _ := crypto.GenerateKey()
	testAddBalance(pool, crypto.PubkeyToAddress(remote.PublicKey), big.NewInt(1000000))
	// Generate and queue a batch of transactions
	txs := types.Transactions{}
	for i := 0; i < 5; i++ {
		txs = append(txs, pricedValuedTransaction(uint64(i), 100000, 100000, big.NewInt(1), remote)) // each cost 100000 + 100000 * 1 = 200000
	}
	// totolly cost 200000 * 5 = 1000000

	// Import the batch and verify that limits have been enforced
	pool.addRemotesSync(txs)

	pending, _ := pool.Stats()

	if pending != txs.Len() {
		t.Fatalf("miss matched queued txs, have: %d, want: %d", pending, txs.Len())
	}

	mute := pricedValuedTransaction(2, 400000, 100000, big.NewInt(2), remote) // cost 400000 + 100000 * 2 = 600000
	pool.addRemote(mute)
	addr := crypto.PubkeyToAddress(remote.PublicKey)

	pend := pool.Pending()
	if len(pend[addr]) != 3 {
		t.Fatalf("miss matched queued txs, have: %d, want: %d", len(pend), 3)
	}

	<-pool.requestReset(nil, nil)
	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}
}
