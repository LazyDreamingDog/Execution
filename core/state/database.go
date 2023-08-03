package state

import (
	"encoding/json"
	"errors"
	"execution/common"
	"execution/common/lru"
	"execution/core/rawdb"
	"execution/core/types"
	"execution/ethdb"
	"fmt"
	"math/big"
)

const (
	// Number of codehash->size associations to keep.
	codeSizeCacheSize = 100000

	// Cache size granted for caching clean code.
	codeCacheSize = 64 * 1024 * 1024
)

type Database interface {
	// ContractCode retrieves a particular contract's code.
	ContractCode(addr common.Address, codeHash common.Hash) ([]byte, error)

	// ContractCodeSize retrieves a particular contracts code's size.
	ContractCodeSize(addr common.Address, codeHash common.Hash) (int, error)

	// DiskDB returns the underlying key-value disk database.
	DiskDB() ethdb.KeyValueStore

	GetAccount(addr common.Address) (*types.StateAccount, error)
	CommitAccount(addr common.Address, metadata []byte, pendingStorage Storage) error
}

func NewDatabase(db ethdb.Database) Database {
	return NewDatabaseWithConfig(db)
}

func NewDatabaseWithConfig(db ethdb.Database) Database {
	return &cachingDB{
		disk:          db,
		codeSizeCache: lru.NewCache[common.Hash, int](codeSizeCacheSize),
		codeCache:     lru.NewSizeConstrainedCache[common.Hash, []byte](codeCacheSize),
	}
}

// 用于做编码存储的数据结构
type storageAccount struct {
	Nonce    uint64
	Balance  *big.Int
	CodeHash []byte
}

type cachingDB struct { // 做一层缓存
	disk          ethdb.KeyValueStore
	codeSizeCache *lru.Cache[common.Hash, int]
	codeCache     *lru.SizeConstrainedCache[common.Hash, []byte]
}

func (db *cachingDB) ContractCode(address common.Address, codeHash common.Hash) ([]byte, error) {
	code, _ := db.codeCache.Get(codeHash)
	if len(code) > 0 {
		return code, nil
	}
	code = rawdb.ReadCode(db.disk, codeHash)
	if len(code) > 0 {
		db.codeCache.Add(codeHash, code)
		db.codeSizeCache.Add(codeHash, len(code))
		return code, nil
	}
	return nil, errors.New("not found")
}

// ContractCodeSize retrieves a particular contracts code's size.
func (db *cachingDB) ContractCodeSize(addr common.Address, codeHash common.Hash) (int, error) {
	if cached, ok := db.codeSizeCache.Get(codeHash); ok {
		return cached, nil
	}
	code, err := db.ContractCode(addr, codeHash)
	return len(code), err
}

func (db *cachingDB) DiskDB() ethdb.KeyValueStore {
	return db.disk
}

// TODO : 可以做一个lru缓存？
func (db *cachingDB) GetAccount(addr common.Address) (*types.StateAccount, error) {
	var acct *types.StateAccount
	// 首先获取全部与Address匹配的KV对
	// acct.Storage = rawdb.ReadStorage(db.disk, addr)
	temp := rawdb.ReadStorage(db.disk, addr)
	if len(temp) == 0 {
		return nil, nil
	}
	for key, value := range rawdb.ReadStorage(db.disk, addr) {
		acct.Storage[key] = value
	}
	// 将Balance和Noce以及codeHash取出
	MetaDataKeyBytes := common.BytesToHash(append(addr.Bytes(), []byte("m")...))
	MetaData := acct.Storage[MetaDataKeyBytes] // 取出metadata的JSON字节数组
	// 格式转换（RLP or JSON）
	// 这里先采用JSON写完读写逻辑，后续根据需求更换为 RLP
	var sA storageAccount
	err := json.Unmarshal(MetaData, &sA)
	if err != nil {
		fmt.Println("Error decoding JSON:", err)
	}
	// 给acct赋值
	acct.Balance = sA.Balance
	acct.Nonce = sA.Nonce
	acct.CodeHash = sA.CodeHash
	return acct, nil
}

func (db *cachingDB) CommitAccount(addr common.Address, metadata []byte, pendingStorage Storage) error {
	stroageWriter := db.disk.NewBatch()
	var err error
	err = rawdb.WriteMetadataToCurrent(stroageWriter, addr, metadata)
	if err != nil {
		return fmt.Errorf("commit error, in metadata")
	}
	for key, value := range pendingStorage {
		err = rawdb.WriteStorageToCurrent(stroageWriter, addr, key, value)
		if err != nil {
			return fmt.Errorf("commit error, in storage data")
		}
	}
	return nil
}
