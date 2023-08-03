package rawdb

import (
	"execution/common"
	"execution/ethdb"
	"execution/log"
	"fmt"
)

// ReadCode retrieves the contract code of the provided code hash.
func ReadCode(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(codeKey(hash))
	return data
}

// WriteCode writes the provided contract code database.
func WriteCode(db ethdb.KeyValueWriter, hash common.Hash, code []byte) {
	if err := db.Put(codeKey(hash), code); err != nil {
		log.Crit("Failed to store contract code", "err", err)
	}
}

// HasCode checks if the contract code corresponding to the
// provided code hash is present in the db.
func HasCode(db ethdb.KeyValueReader, hash common.Hash) bool {
	// Try with the prefixed code scheme first, if not then try with legacy
	// scheme.
	// if ok := HasCodeWithPrefix(db, hash); ok {
	// 	return true
	// }
	ok, _ := db.Has(codeKey(hash))
	return ok
}

func WriteMetadataToCurrent(db ethdb.KeyValueWriter, addr common.Address, metadata []byte) error {
	if err := db.Put(metadataKey(addr), metadata); err != nil {
		log.Crit("Failed to store account metadata", "err", err)
		return fmt.Errorf("Failed to store account metadata")
	}
	return nil
}

func WriteStorageToCurrent(db ethdb.KeyValueWriter, addr common.Address, key common.Hash, value common.Hash) error {
	if err := db.Put(storageKey(addr, key), value[:]); err != nil {
		log.Crit("Failed to store account metadata", "err", err)
		return fmt.Errorf("Failed to store account storage data")
	}
	return nil
}

func ReadMetadata(db ethdb.KeyValueReader, addr common.Address) []byte {
	data, _ := db.Get(metadataKey(addr))
	return data
}

func ReadStorage(db ethdb.KeyValueStore, addr common.Address) map[common.Hash][]byte {
	prefix := addr.Bytes()
	iter := db.NewIterator(prefix, nil)
	if iter.Error() != nil {
		return nil
	}
	result := make(map[common.Hash][]byte)
	for iter.Next() {
		key := iter.Key()
		value := iter.Value()
		fmt.Printf("Key: %s, Value: %s\n", key, value) // 测试用
		result[common.BytesToHash(key)] = value
	}
	return result
}

func WriteMetadataToHistory(db ethdb.KeyValueWriter, key []byte, metadata []byte) error {
	if err := db.Put(key, metadata); err != nil {
		log.Crit("Failed to store account metadata", "err", err)
		return fmt.Errorf("Failed to store account metadata")
	}
	return nil
}

func WriteStorageToHistory(db ethdb.KeyValueWriter, key []byte, value common.Hash) error {
	if err := db.Put(key, value[:]); err != nil {
		log.Crit("Failed to store account metadata", "err", err)
		return fmt.Errorf("Failed to store account storage data")
	}
	return nil
}
