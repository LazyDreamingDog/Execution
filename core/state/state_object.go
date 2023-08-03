package state

import (
	"bytes"
	"encoding/json"
	"execution/common"
	"execution/core/types"
	"execution/crypto"
	"execution/rlp"
	"fmt"
	"io"
	"math/big"
)

type Code []byte

func (c Code) String() string {
	return string(c) //strings.Join(Disassemble(c), " ")
}

type Storage map[common.Hash]common.Hash

func (s Storage) String() (str string) {
	for key, value := range s {
		str += fmt.Sprintf("%X : %X\n", key, value)
	}
	return
}

func (s Storage) Copy() Storage {
	cpy := make(Storage, len(s))
	for key, value := range s {
		cpy[key] = value
	}
	return cpy
}

// type StorageRecord struct {
// 	Key, Value common.Hash
// }

// 记录每一笔交易的metadata修改信息
type MetadataRecord struct {
	Nonce    uint64
	Balance  *big.Int
	CodeHash []byte
	Code     []byte
}

type stateObject struct {
	db       *StateDB
	address  common.Address      // 账户地址
	addrHash common.Hash         // 账户地址哈希
	origin   *types.StateAccount // 原始账户（不作任何修改），nil表示不存在
	data     types.StateAccount  // 状态账户，暂存与账户相关的所有状态

	code Code // 合约bytecode

	originStorage  Storage // Storage cache of original entries to dedup rewrites
	pendingStorage Storage // Storage entries that need to be flushed to disk, at the end of an entire block
	dirtyStorage   Storage // Storage entries that have been modified in the current transaction execution, reset for every transaction

	// 记录每个交易修改的东西
	storageRecord  map[int]Storage        // 每次被SetState时被调用
	metadataRecord map[int]MetadataRecord // TxIndex对应下的账户原始数据修改

	// Cache flags.
	dirtyCode bool // 合约Code是否修改的标志

	// Flag whether the account was marked as suicided. The suicided account
	// is still accessible in the scope of same transaction.
	suicided bool

	// Flag whether the account was marked as deleted. The suicided account
	// or the account is considered as empty will be marked as deleted at
	// the end of transaction and no longer accessible anymore.
	deleted bool
}

func (s *stateObject) empty() bool {
	return s.data.Nonce == 0 && s.data.Balance.Sign() == 0 && bytes.Equal(s.data.CodeHash, types.EmptyCodeHash.Bytes()) && len(s.data.Storage) == 0
}

func newObject(db *StateDB, address common.Address, acct *types.StateAccount) *stateObject {
	origin := acct
	if acct == nil {
		acct = types.NewEmptyStateAccount()
	}
	return &stateObject{
		db:             db,
		address:        address,
		addrHash:       crypto.Keccak256Hash(address[:]),
		origin:         origin,
		data:           *acct,
		originStorage:  make(Storage),
		pendingStorage: make(Storage),
		dirtyStorage:   make(Storage),
		storageRecord:  make(map[int]Storage),
		metadataRecord: make(map[int]MetadataRecord),
	}

}

func (s *stateObject) deepCopy(db *StateDB) *stateObject {
	obj := &stateObject{
		db:       db,
		address:  s.address,
		addrHash: s.addrHash,
		origin:   s.origin,
		data:     s.data,
	}
	obj.code = s.code
	obj.dirtyStorage = s.dirtyStorage.Copy()
	obj.originStorage = s.originStorage.Copy()
	obj.pendingStorage = s.pendingStorage.Copy()
	obj.suicided = s.suicided
	obj.dirtyCode = s.dirtyCode
	obj.deleted = s.deleted
	return obj
}

// EncodeRLP implements rlp.Encoder.
func (s *stateObject) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &s.data)
}

func (s *stateObject) markSuicided() {
	s.suicided = true
}

// GetState retrieves a value from the account storage trie.
func (s *stateObject) GetState(db Database, key common.Hash) common.Hash {
	// 若key对应的value已修改过存在于内存中
	value, dirty := s.dirtyStorage[key]
	if dirty {
		return value
	}
	// 否则要去进一步去获取
	return s.GetCommittedState(db, key)
}

func (s *stateObject) GetCommittedState(db Database, key common.Hash) common.Hash {
	// 在提交队列和缓存队列中去找
	if value, pending := s.pendingStorage[key]; pending {
		return value
	}
	if value, cached := s.originStorage[key]; cached {
		return value
	}
	// TODO : 快照
	// 若都找不到则去数据库中找
	var value common.Hash
	val, err := s.GetOriginStorage(s.address, key.Bytes())
	if err != nil {
		s.db.setError(err)
		return common.Hash{}
	}
	value.SetBytes(val)
	s.originStorage[key] = value
	return value
}

// GetStorage 取合约状态数据
func (s *stateObject) GetOriginStorage(addr common.Address, key []byte) ([]byte, error) {
	value, flag := s.data.Storage[common.BytesToHash(append(addr.Bytes(), key...))]
	if flag {
		return value, nil
	} else {
		return []byte{}, fmt.Errorf("key not found")
	}
}

// SetState 更新key-value状态到数据库（给statedb.go调用）
func (s *stateObject) SetState(db Database, key, value common.Hash) {
	// 如果新value与旧的相同则直接返回
	prev := s.GetState(db, key)
	if prev == value {
		return
	}
	s.db.journal.append(storageChange{
		account:  &s.address,
		key:      key,
		prevalue: prev,
	})

	s.storageRecord[s.db.txIndex][key] = value // 记录中间状态（每一笔交易对应）

	s.setState(key, value)
}

// setState 将key-value暂存到内存的存储条
func (s *stateObject) setState(key, value common.Hash) {
	s.dirtyStorage[key] = value
}

// Balance 返回账户余额（读取余额）
func (s *stateObject) Balance() *big.Int {
	return s.data.Balance
}

// AddBalance 用于转账时增加s中account的余额
func (s *stateObject) AddBalance(amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	s.SetBalance(new(big.Int).Add(s.Balance(), amount))
}

// AddBalance 用于转账时减去s中account的余额
func (s *stateObject) SubBalance(amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	s.SetBalance(new(big.Int).Sub(s.Balance(), amount))
}

// SetBalance 将余额更新到状态数据库
func (s *stateObject) SetBalance(amount *big.Int) {
	// s.SetState(db, common.BytesToHash([]byte{byte('B')}), common.BigToHash(amount))
	s.db.journal.append(balanceChange{
		account: &s.address,
		prev:    new(big.Int).Set(s.data.Balance),
	})
	// 存储balance的中间状态
	if _, ok := s.metadataRecord[s.db.txIndex]; ok { // 若存在
		temp := &MetadataRecord{
			Balance:  amount,
			Nonce:    s.metadataRecord[s.db.txIndex].Nonce,
			Code:     s.metadataRecord[s.db.txIndex].Code,
			CodeHash: s.metadataRecord[s.db.txIndex].CodeHash[:],
		}
		s.metadataRecord[s.db.txIndex] = *temp
	} else {
		temp := &MetadataRecord{
			Balance:  amount,
			Nonce:    uint64(0),
			Code:     make([]byte, 0),
			CodeHash: make([]byte, 0),
		}
		s.metadataRecord[s.db.txIndex] = *temp
	}
	s.setBalance(amount)
}

// 暂存到内存中(再考虑key是否处理:在上一步中已做处理)
func (s *stateObject) setBalance(amount *big.Int) {
	s.data.Balance = amount
}

func (s *stateObject) SetCode(codeHash common.Hash, code []byte) {
	prevcode := s.Code(s.db.currentDB)
	s.db.journal.append(codeChange{
		account:  &s.address,
		prevhash: s.CodeHash(),
		prevcode: prevcode,
	})

	// 存储code的中间状态
	if _, ok := s.metadataRecord[s.db.txIndex]; ok { // 若存在
		temp := &MetadataRecord{
			Balance:  s.metadataRecord[s.db.txIndex].Balance,
			Nonce:    s.metadataRecord[s.db.txIndex].Nonce,
			Code:     code,
			CodeHash: codeHash[:],
		}
		s.metadataRecord[s.db.txIndex] = *temp
	} else {
		temp := &MetadataRecord{
			Balance:  new(big.Int),
			Nonce:    uint64(0),
			Code:     code,
			CodeHash: codeHash[:],
		}
		s.metadataRecord[s.db.txIndex] = *temp
	}
	s.setCode(codeHash, code)
}

func (s *stateObject) setCode(codeHash common.Hash, code []byte) {
	s.code = code
	s.data.CodeHash = codeHash[:]
	s.dirtyCode = true
}

func (s *stateObject) CodeHash() []byte {
	return s.data.CodeHash
}

// Code 返回合约的bytecode
func (s *stateObject) Code(db Database) []byte {
	// 如果内存中有则直接取了返回
	if s.code != nil {
		return s.code
	}
	code, err := db.ContractCode(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
	}
	s.code = code
	return code
}

func (s *stateObject) CodeSize(db Database) int {
	if s.code != nil {
		return len(s.code)
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return 0
	}
	size, err := db.ContractCodeSize(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	return size
}

// Nonce 返回账户的nonce
func (s *stateObject) Nonce() uint64 {
	return s.data.Nonce
}

func (s *stateObject) SetNonce(nonce uint64) {
	s.db.journal.append(nonceChange{
		account: &s.address,
		prev:    s.data.Nonce,
	})

	// 存储nonce的中间状态
	if _, ok := s.metadataRecord[s.db.txIndex]; ok { // 若存在
		temp := &MetadataRecord{
			Balance:  s.metadataRecord[s.db.txIndex].Balance,
			Nonce:    nonce,
			Code:     s.metadataRecord[s.db.txIndex].Code,
			CodeHash: s.metadataRecord[s.db.txIndex].CodeHash[:],
		}
		s.metadataRecord[s.db.txIndex] = *temp
	} else {
		temp := &MetadataRecord{
			Balance:  new(big.Int),
			Nonce:    nonce,
			Code:     make([]byte, 0),
			CodeHash: make([]byte, 0),
		}
		s.metadataRecord[s.db.txIndex] = *temp
	}
	s.setNonce(nonce)
}

func (s *stateObject) setNonce(nonce uint64) {
	s.data.Nonce = nonce
}

// Address 返回 合约/用户账户 的地址
func (s *stateObject) Address() common.Address {
	return s.address
}

func (s *stateObject) finalise() {
	// slotsToPrefetch := make([][]byte, 0, len(s.dirtyStorage))
	for key, value := range s.dirtyStorage {
		s.pendingStorage[key] = value
		// if value != s.originStorage[key] {
		// 	slotsToPrefetch = append(slotsToPrefetch, common.CopyBytes(key[:])) // Copy needed for closure
		// }
	}
	// if s.db.prefetcher != nil && prefetch && len(slotsToPrefetch) > 0 && s.data.Root != types.EmptyRootHash {
	// 	s.db.prefetcher.prefetch(s.addrHash, s.data.Root, s.address, slotsToPrefetch)
	// }
	if len(s.dirtyStorage) > 0 {
		s.dirtyStorage = make(Storage)
	}
}

// commit 提交状态账户的数据
func (s *stateObject) commit(db Database) error {
	// // finalise一下，把dirty放到pending（待确定是否需要，暂时用着）
	// s.finalise()	// 在stateDB的commit的Finalise已经被调用
	// 提交全部数据
	// 将Nonce, Balance, codeHash提交存储
	var sA storageAccount
	sA.Nonce = s.data.Nonce
	sA.Balance = s.data.Balance
	sA.CodeHash = s.data.CodeHash

	// JSON 编码 （后续考虑修改为RLP）
	metaData, err := json.Marshal(sA)
	if err != nil {
		return fmt.Errorf("error encoding to json")
	}
	// 提交pending到WriteSet
	for key, value := range s.pendingStorage {
		s.db.writeSet[key] = value
	}
	// 提交metadata 和 pendingStorage到当前状态数据库
	err = db.CommitAccount(s.address, metaData, s.pendingStorage)
	if err != nil {
		return fmt.Errorf("commit error, in stateObject commit")
	}
	return nil
}

// commitHistory 传入 区块号 和 历史状态数据库 ，将obj中的metadataRecord和storageRecord全部提交到历史状态数据库中
func (s *stateObject) commitHistory(BlockNum uint64, db *HistoryDB) error {
	// 提交 metadata 和 dirtyStorage 到当前状态数据库
	err := db.CommitAccountToHistory(s.address, BlockNum, s.storageRecord, s.metadataRecord)
	if err != nil {
		return fmt.Errorf("commit error, in stateObject commit")
	}
	return nil
}
