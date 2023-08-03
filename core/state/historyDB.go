package state

import (
	"encoding/binary"
	"encoding/json"
	"execution/common"
	"execution/core/rawdb"
	"execution/ethdb"
	"fmt"
)

type HistoryDB struct {
	disk ethdb.KeyValueStore
}

func NewHistoryDB(db ethdb.Database) *HistoryDB {
	return &HistoryDB{
		disk: db,
	}
}
func (hdb *HistoryDB) CommitAccountToHistory(addr common.Address, BlockNum uint64, storageRecord map[int]Storage, metadataRecord map[int]MetadataRecord) error {
	stroageWriter := hdb.disk.NewBatch()
	var err error
	// 处理MetaData：Balance，Nonce，Code，CodeHash
	for txId, metadata := range metadataRecord {
		key := metadataHistoryKey(BlockNum, txId, addr)                       // 生成Matadata的存储Key
		metaDataBytes, _ := json.Marshal(metadata)                            // 用JSON序列化Metadata的内容
		err = rawdb.WriteMetadataToHistory(stroageWriter, key, metaDataBytes) // 写入数据库
		if err != nil {
			return fmt.Errorf("commit error, in metadata")
		}
	}

	for txId, stroageList := range storageRecord {
		for sKey, sVal := range stroageList { // 遍历一个TxId下的所有修改过的slot
			key := storageHistoryKey(BlockNum, txId, addr, sKey)        // 生成存储Key
			err = rawdb.WriteStorageToHistory(stroageWriter, key, sVal) // 写入数据库
			if err != nil {
				return fmt.Errorf("commit error, in storage data")
			}
		}
	}
	return nil
}

// storageHistoryKey 生成合约状态数据在历史数据库中的存储Key
func storageHistoryKey(bn uint64, txId int, addr common.Address, key common.Hash) []byte {
	result := make([]byte, 0)
	result = append(result, addr.Bytes()...) // addr
	result = append(result, key.Bytes()...)  //addr + key
	// 转换block number
	byteDataBn := make([]byte, 8)
	binary.BigEndian.PutUint64(byteDataBn, bn)
	result = append(result, byteDataBn...) // addr + key + blocknumber

	// 转换TxIndex
	byteDataTxId := make([]byte, 4)
	binary.BigEndian.PutUint32(byteDataTxId, uint32(txId))
	result = append(result, byteDataTxId...) // addr + key + blocknumber + TxIndex
	return result
}

func metadataHistoryKey(bn uint64, txId int, addr common.Address) []byte {
	result := make([]byte, 0)
	result = append(result, addr.Bytes()...)         // addr
	result = append(result, rawdb.MetadataPrefix...) // addr + 'm'

	// 转换block number
	byteDataBn := make([]byte, 8)
	binary.BigEndian.PutUint64(byteDataBn, bn)
	result = append(result, byteDataBn...) // addr + 'm' + blocknumber

	// 转换TxIndex
	byteDataTxId := make([]byte, 4)
	binary.BigEndian.PutUint32(byteDataTxId, uint32(txId))
	result = append(result, byteDataTxId...) // addr + 'm' + blocknumber + TxIndex
	return result
}

// // submitToOnChainStorage 将打开的historyDB的全部内容提交到链上存储区进行存储
// func (hdb *HistoryDB) submitToOnChainStorage() error {

// }
