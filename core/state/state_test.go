package state

import (
	"execution/common"
	"execution/crypto"
	"execution/ethdb"
	"fmt"
	"math/big"
	"testing"

	"execution/core/rawdb"
)

type stateEnv struct {
	currentDB ethdb.Database
	historyDB ethdb.Database
	state     *StateDB
}

func newStateEnv() *stateEnv {
	db1 := rawdb.NewMemoryDatabase() // currentDB
	db2 := rawdb.NewMemoryDatabase() // historyDB
	sdb, _ := New(NewDatabase(db1), NewHistoryDB(db2))
	return &stateEnv{currentDB: db1, historyDB: db2, state: sdb}
}

func TestNull(t *testing.T) {
	s := newStateEnv()
	fmt.Println(s)
	// generate a few entries
	obj1 := s.state.GetOrNewStateObject(common.BytesToAddress([]byte{0x01}))
	fmt.Println(obj1)
	obj1.AddBalance(big.NewInt(22))
	fmt.Println(obj1.data.Balance)
	// obj2 := s.state.GetOrNewStateObject(common.BytesToAddress([]byte{0x01}))
	obj2 := s.state.GetOrNewStateObject(common.BytesToAddress([]byte{0x01, 0x02}))
	fmt.Println(obj2)
	obj2.SetCode(crypto.Keccak256Hash([]byte{3, 3, 3, 3, 3, 3, 3}), []byte{3, 3, 3, 3, 3, 3, 3})
	obj3 := s.state.GetOrNewStateObject(common.BytesToAddress([]byte{0x02}))
	obj3.SetBalance(big.NewInt(44))
	obj4 := s.state.GetOrNewStateObject(common.BytesToAddress([]byte{0x00}))
	obj4.AddBalance(big.NewInt(1337))
}

func TestSet(t *testing.T) {

}
