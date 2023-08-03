package state

import (
	"execution/common"
	"execution/core/rawdb"
	"execution/core/types"
	"execution/crypto"
	"execution/params"
	"fmt"
	"math/big"
)

type StateDB struct {
	// 用于存储账户状态的两个数据库（当前状态 and 历史状态）
	currentDB Database

	historyDB *HistoryDB

	// // 与状态修改相关
	// accounts map[common.Hash][]byte

	// 内存中与状态修改相关的，边执行边处理
	stateObjects        map[common.Address]*stateObject // 读写过状态的账户集合
	stateObjectsPending map[common.Address]struct{}     // State objects finalized but not yet written to the trie
	stateObjectsDirty   map[common.Address]struct{}     // 当前区块被修改过的账户集合

	// 用于并行执行的访问控制列表
	accessList *accessList

	// 整合后的写集
	writeSet map[common.Hash]common.Hash

	journal *journal

	// The Tx Context
	thash   common.Hash
	txIndex int

	// Block Info
	blockNum uint64

	// The refund counter, also used by state transitioning.
	refund uint64

	dbErr error
}

/*
StateDB 的新建与复制操作
*/
func New(currentDB Database, historyDB *HistoryDB) (*StateDB, error) {
	sdb := &StateDB{
		currentDB:           currentDB,
		historyDB:           historyDB,
		stateObjects:        make(map[common.Address]*stateObject),
		stateObjectsPending: make(map[common.Address]struct{}),
		stateObjectsDirty:   make(map[common.Address]struct{}),
		journal:             newJournal(),
		accessList:          newAccessList(),
	}
	// if sdb.snaps != nil {
	// 	sdb.snap = sdb.snaps.Snapshot(root)
	// }
	return sdb, nil
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (sdb *StateDB) Copy() *StateDB {
	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		currentDB:           sdb.currentDB,
		historyDB:           sdb.historyDB,
		stateObjects:        make(map[common.Address]*stateObject, len(sdb.journal.dirties)),
		stateObjectsPending: make(map[common.Address]struct{}, len(sdb.stateObjectsPending)),
		stateObjectsDirty:   make(map[common.Address]struct{}, len(sdb.journal.dirties)),
		refund:              sdb.refund,
		// logs:                 make(map[common.Hash][]*types.Log, len(s.logs)),
		// logSize:              s.logSize,
		// preimages:            make(map[common.Hash][]byte, len(s.preimages)),
		journal: newJournal(),
		// hasher:               crypto.NewKeccakState(),

		// In order for the block producer to be able to use and make additions
		// to the snapshot tree, we need to copy that as well. Otherwise, any
		// block mined by ourselves will cause gaps in the tree, and force the
		// miner to operate trie-backed only.
		// snaps: s.snaps,
		// snap:  s.snap,
	}
	// Copy the dirty states, logs, and preimages
	for addr := range sdb.journal.dirties {
		if object, exist := sdb.stateObjects[addr]; exist {
			state.stateObjects[addr] = object.deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}   // Mark the copy dirty to force internal (code/state) commits
			state.stateObjectsPending[addr] = struct{}{} // Mark the copy pending to force external (account) commits
		}
	}
	// Above, we don't copy the actual journal. This means that if the copy
	// is copied, the loop above will be a no-op, since the copy's journal
	// is empty. Thus, here we iterate over stateObjects, to enable copies
	// of copies.
	for addr := range sdb.stateObjectsPending {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = sdb.stateObjects[addr].deepCopy(state)
		}
		state.stateObjectsPending[addr] = struct{}{}
	}
	for addr := range sdb.stateObjectsDirty {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = sdb.stateObjects[addr].deepCopy(state)
		}
		state.stateObjectsDirty[addr] = struct{}{}
	}
	// // Deep copy the state changes made in the scope of block
	// // along with their original values.
	// state.accounts = copyAccounts(s.accounts)
	// state.storages = copyStorages(s.storages)
	// state.accountsOrigin = copyAccounts(state.accountsOrigin)
	// state.storagesOrigin = copyStorages(state.storagesOrigin)

	// Deep copy the logs occurred in the scope of block
	// for hash, logs := range s.logs {
	// 	cpy := make([]*types.Log, len(logs))
	// 	for i, l := range logs {
	// 		cpy[i] = new(types.Log)
	// 		*cpy[i] = *l
	// 	}
	// 	state.logs[hash] = cpy
	// }
	// Deep copy the preimages occurred in the scope of block
	// for hash, preimage := range s.preimages {
	// 	state.preimages[hash] = preimage
	// }
	// Do we need to copy the access list and transient storage?
	// In practice: No. At the start of a transaction, these two lists are empty.
	// In practice, we only ever copy state _between_ transactions/blocks, never
	// in the middle of a transaction. However, it doesn't cost us much to copy
	// empty lists, so we do it anyway to not blow up if we ever decide copy them
	// in the middle of a transaction.
	state.accessList = sdb.accessList.Copy()
	// state.transientStorage = s.transientStorage.Copy()

	// If there's a prefetcher running, make an inactive copy of it that can
	// only access data but does not actively preload (since the user will not
	// know that they need to explicitly terminate an active copy).
	// if s.prefetcher != nil {
	// 	state.prefetcher = s.prefetcher.copy()
	// }
	return state
}

/*
执行与状态读写相关的操作
*/

// GetState 从给定账户地址中获取key对应的value
func (sdb *StateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	stateObject := sdb.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(sdb.currentDB, key) // 从当前状态数据库获取
	}
	return common.Hash{}
}

func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetCommittedState(s.currentDB, hash)
	}
	return common.Hash{}
}

// SetState 将对应账户的Key-Value写入数据库中
func (sdb *StateDB) SetState(addr common.Address, key, value common.Hash) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetState(sdb.currentDB, key, value)
	}
}

// AddBalance 对addr相关的账户余额执行加操作
func (sdb *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddBalance(amount)
	}
}

// SubBalance 对addr相关的账户余额执行减操作
func (sdb *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
	}
}

func (sdb *StateDB) SetBalance(addr common.Address, amount *big.Int) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}

func (s *StateDB) GetBalance(addr common.Address) *big.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.Big0
}

func (sdb *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (s *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}
	return 0
}

func (sdb *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := sdb.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}
}

func (s *StateDB) GetCode(addr common.Address) []byte {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(s.currentDB)
	}
	return nil
}

func (s *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.CodeSize(s.currentDB)
	}
	return 0
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return common.Hash{}
	}
	return common.BytesToHash(stateObject.CodeHash())
}

/*
执行提交状态数据到持久化存储的操作
*/

func (sdb *StateDB) Finalise() {
	// addressesToPrefetch := make([][]byte, 0, len(sdb.journal.dirties))
	for addr := range sdb.journal.dirties {
		obj, exist := sdb.stateObjects[addr]
		if !exist {
			continue // 防止将回滚的交易数据提交
		}
		// TODO : 添加删除销毁的逻辑
		obj.finalise()
		sdb.stateObjectsPending[addr] = struct{}{}
		sdb.stateObjectsDirty[addr] = struct{}{}
	}
	sdb.clearJournalAndRefund()
}

// Commit 将StateDB中修改的内容提交到数据库中
// 每个区块执行结束后被调用
// 返回区块的状态修改hash，以及可能存在的错误
// TODO : 补充加入历史数据库的逻辑 （***）
func (sdb *StateDB) Commit() (common.Hash, error) {
	// 错误检测
	if sdb.dbErr != nil {
		return common.Hash{}, fmt.Errorf("commit aborted due to earlier error")
	}
	var codeWriter = sdb.currentDB.DiskDB().NewBatch()

	sdb.Finalise()

	// 提交到数据库中
	for addr := range sdb.stateObjectsDirty {
		obj := sdb.stateObjects[addr]
		if obj.deleted {
			continue // 若账户被标记为删除
		} // TODO : 补充删除逻辑
		if obj.code != nil && obj.dirtyCode { // 创建合约的时候dirtycode才为1？
			rawdb.WriteCode(codeWriter, common.BytesToHash(obj.CodeHash()), obj.code)
			obj.dirtyCode = false
		}
		// 把账户状态数据分别写入当前状态数据库和历史状态数据库
		err := obj.commit(sdb.currentDB)
		if err != nil {
			return common.Hash{}, err
		}
		err = obj.commitHistory(sdb.blockNum, sdb.historyDB)
		if err != nil {
			return common.Hash{}, err
		}
	}
	// 对写集计算哈希根返回
	var hashBytes []byte
	for _, value := range sdb.writeSet {
		hashBytes = append(hashBytes, value.Bytes()...)
	}
	verifyHash := crypto.Keccak256Hash(hashBytes)

	return verifyHash, nil
}

/*
执行生成和获取stateObject相关的操作
*/

// setStateObject 将给定的stateObject存到stateDB的列表中
func (sdb *StateDB) setStateObject(object *stateObject) {
	sdb.stateObjects[object.Address()] = object
}

// getStateObject 读取给定地址对应的stateObject
func (sdb *StateDB) getStateObject(addr common.Address) *stateObject {
	if obj := sdb.getDeletedStateObject(addr); obj != nil && !obj.deleted {
		return obj
	}
	return nil
}

// TODO : 大逻辑->当删除合约账户后将所有数据库中的全部内容删除
// 这里暂时沿用以太坊中删除账户将其的deleted标志置true的操作

// getDeletedStateObject
func (sdb *StateDB) getDeletedStateObject(addr common.Address) *stateObject {
	// 尝试在当前内存中找
	if obj := sdb.stateObjects[addr]; obj != nil {
		return obj
	}
	// 若内存中没有，从数据库中获取
	// TODO:补充从快照获取
	var data *types.StateAccount
	var err error
	data, err = sdb.currentDB.GetAccount(addr)
	if err != nil {
		sdb.setError(fmt.Errorf("getDeleteStateObject (%x) error: %w", addr.Bytes(), err))
		return nil
	}
	if data == nil {
		return nil
	}
	// 将获取到的obj缓存到内存中
	obj := newObject(sdb, addr, data)
	sdb.setStateObject(obj)
	return obj
}

// GetOrNewStateObject 读取或创建（新建账户时）给定地址对应的stateObject
func (sdb *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	stateObject := sdb.getStateObject(addr)
	if stateObject == nil { // 如果读取为空则创建
		stateObject, _ = sdb.createObject(addr)
	}
	return stateObject
}

// createObject 创建一个新的stateObject
// 以太坊中可能会创建到一个已经被标记为deleted的账户
// TODO : 考虑基于prev提高程序健壮性和完善逻辑
func (sdb *StateDB) createObject(addr common.Address) (newobj, prev *stateObject) {
	prev = sdb.getDeletedStateObject(addr) // Note, prev might have been deleted, we need that!
	newobj = newObject(sdb, addr, nil)
	//
	sdb.setStateObject(newobj)
	if prev != nil && !prev.deleted {
		return newobj, prev
	}
	return newobj, nil
}

func (sdb *StateDB) CreateAccount(addr common.Address) {
	newObj, prev := sdb.createObject(addr)
	if prev != nil {
		newObj.setBalance(prev.data.Balance)
	}
}

/*
TODO : 执行与快照相关的操作
*/
// 暂时置成空函数
// Snapshot returns an identifier for the current revision of the state.
func (s *StateDB) Snapshot() int {
	// id := s.nextRevisionId
	// s.nextRevisionId++
	// s.validRevisions = append(s.validRevisions, revision{id, s.journal.length()})
	// return id
	return 0
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (s *StateDB) RevertToSnapshot(revid int) {
	// // Find the snapshot in the stack of valid snapshots.
	// idx := sort.Search(len(s.validRevisions), func(i int) bool {
	// 	return s.validRevisions[i].id >= revid
	// })
	// if idx == len(s.validRevisions) || s.validRevisions[idx].id != revid {
	// 	panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	// }
	// snapshot := s.validRevisions[idx].journalIndex

	// // Replay the journal to undo changes and remove invalidated snapshots
	// s.journal.revert(s, snapshot)
	// s.validRevisions = s.validRevisions[:idx]
}

/*
AccessList 相关操作
*/
func (s *StateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) {
	if rules.IsBerlin || true {
		// Clear out any leftover from previous executions
		al := newAccessList()
		s.accessList = al

		al.AddAddress(sender)
		if dst != nil {
			al.AddAddress(*dst)
			// If it's a create-tx, the destination will be added inside evm.create
		}
		for _, addr := range precompiles {
			al.AddAddress(addr)
		}

		for _, el := range list {
			al.AddAddress(el.Address)
			for _, key := range el.StorageKeys {
				al.AddSlot(el.Address, key)
			}
		}
		// if rules.IsShanghai { // EIP-3651: warm coinbase
		// 	al.AddAddress(coinbase)
		// }
	}
	// Reset transient storage at the beginning of transaction execution
	// s.transientStorage = newTransientStorage()
}

// AddAddressToAccessList adds the given address to the access list
func (s *StateDB) AddAddressToAccessList(addr common.Address) {
	s.accessList.AddAddress(addr)
}

// AddSlotToAccessList adds the given (address, slot)-tuple to the access list
func (s *StateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	// addrMod, slotMod := s.accessList.AddSlot(addr, slot)
	s.accessList.AddSlot(addr, slot)
}

// AddressInAccessList returns true if the given address is in the access list.
func (s *StateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.ContainsAddress(addr)
}

// SlotInAccessList returns true if the given (address, slot)-tuple is in the access list.
func (s *StateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressPresent bool, slotPresent bool) {
	return s.accessList.Contains(addr, slot)
}

/*
杂项操作
*/

func (sdb *StateDB) setError(err error) {
	if sdb.dbErr == nil {
		sdb.dbErr = err
	}
}

// Error returns the memorized database failure occurred earlier.
func (sdb *StateDB) Error() error {
	return sdb.dbErr
}

func (sdb *StateDB) clearJournalAndRefund() {
	if len(sdb.journal.entries) > 0 {
		sdb.journal = newJournal()
		// s.refund = 0
	}
	// s.validRevisions = s.validRevisions[:0] // Snapshots can be created without journal entries
}

// SetTxContext sets the current transaction hash and index which are
// used when the EVM emits new state logs. It should be invoked before
// transaction execution.
func (sdb *StateDB) SetTxContext(thash common.Hash, ti int) {
	sdb.thash = thash
	sdb.txIndex = ti
}

func (sdb *StateDB) SetBlockInfo(blockNum uint64) {
	sdb.blockNum = blockNum
}

/*
Log操作
*/
// 暂时全部 置成空函数
func (s *StateDB) AddLog(log *types.Log) {
	// s.journal.append(addLogChange{txhash: s.thash})

	// log.TxHash = s.thash
	// log.TxIndex = uint(s.txIndex)
	// log.Index = s.logSize
	// s.logs[s.thash] = append(s.logs[s.thash], log)
	// s.logSize++
}

// GetLogs returns the logs matching the specified transaction hash, and annotates
// them with the given blockNumber and blockHash.
func (s *StateDB) GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash) []*types.Log {
	// logs := s.logs[hash]
	// for _, l := range logs {
	// 	l.BlockNumber = blockNumber
	// 	l.BlockHash = blockHash
	// }
	// return logs
	return nil
}

func (s *StateDB) Logs() []*types.Log {
	// var logs []*types.Log
	// for _, lgs := range s.logs {
	// 	logs = append(logs, lgs...)
	// }
	// return logs
	return nil
}

/*
Refund 操作
*/
// 暂时置成空函数
// AddRefund adds gas to the refund counter
func (s *StateDB) AddRefund(gas uint64) {
	// s.journal.append(refundChange{prev: s.refund})
	s.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (s *StateDB) SubRefund(gas uint64) {
	// s.journal.append(refundChange{prev: s.refund})
	if gas > s.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, s.refund))
	}
	s.refund -= gas
}

func (s *StateDB) GetRefund() uint64 {
	return s.refund
}

/*
暂时用不上
*/
// Suicide marks the given account as suicided.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after Suicide.
func (s *StateDB) Suicide(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return false
	}
	// s.journal.append(suicideChange{
	// 	account:     &addr,
	// 	prev:        stateObject.suicided,
	// 	prevbalance: new(big.Int).Set(stateObject.Balance()),
	// })
	stateObject.markSuicided()
	stateObject.data.Balance = new(big.Int)
	return true
}

func (s *StateDB) HasSuicided(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.suicided
	}
	return false
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *StateDB) Empty(addr common.Address) bool {
	so := s.getStateObject(addr)
	return so == nil || so.empty()
}

func (s *StateDB) Exist(addr common.Address) bool {
	return s.getStateObject(addr) != nil
}

// setTransientState is a lower level setter for transient storage. It
// is called during a revert to prevent modifications to the journal.
func (s *StateDB) setTransientState(addr common.Address, key, value common.Hash) {
	// s.transientStorage.Set(addr, key, value)
}

// GetTransientState gets transient storage for a given account.
func (s *StateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	// return s.transientStorage.Get(addr, key)
	return common.Hash{}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (s *StateDB) Preimages() map[common.Hash][]byte {
	// return s.preimages
	return nil
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	// if _, ok := s.preimages[hash]; !ok {
	// 	s.journal.append(addPreimageChange{hash: hash})
	// 	pi := make([]byte, len(preimage))
	// 	copy(pi, preimage)
	// 	s.preimages[hash] = pi
	// }
}
