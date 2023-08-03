package types

import (
	"crypto/ecdsa"
	"encoding/json"
	"execution/common"
	"execution/core/types/gadget"
	"execution/crypto"
	"execution/params"
	"math"
	"math/big"
)

type TxType uint8

const (
	NormalTx TxType = iota
	WithdrawTx
	RechargeTx
	UnkownTx
)

type Transaction struct {
	TxPreface
	TxInner
	TxExtends
}

type TxPreface struct {
	TxHash     common.Hash        `json:"txHash,omitempty"`
	From       common.Address     `json:"from,omitempty"`
	Nonce      uint64             `json:"nonce,omitempty"`
	GasLimit   uint64             `json:"gasLimit,omitempty"`
	GasPrice   *gadget.GasPrice   `json:"gasPrice,omitempty"`
	Value      *big.Int           `json:"value,omitempty"`
	Validation *gadget.Validation `json:"validation,omitempty"`

	InputCoins  []gadget.InputCoin  `json:"inputCoins,omitempty"`
	Witnesses   []gadget.Witness    `json:"witenesses,omitempty"`
	OutputCoins []gadget.OutputCoin `json:"outputCoins,omitempty"`
}

type TxInner struct {
	To         common.Address     `json:"to,omitempty"`
	Data       []byte             `json:"data,omitempty"`
	AccessList *gadget.AccessList `json:"accessList,omitempty"`
}

type TxExtends struct {
	Refund           *gadget.Refund     `json:"refund,omitempty"`
	Extend           []byte             `json:"extend,omitempty"`
	StrictAccessList *gadget.AccessList `json:"strictAccessList,omitempty"`
}

func (tx *Transaction) Type() TxType {
	if (tx.From != common.Address{}) {
		if tx.InputCoins == nil {
			return NormalTx
		} else {
			return UnkownTx
		}
	} else {
		if tx.InputCoins == nil && tx.OutputCoins != nil {
			return WithdrawTx
		}
		if tx.InputCoins != nil && tx.OutputCoins == nil {
			return RechargeTx
		}
	}
	return UnkownTx
}

func (tx *Transaction) Serialize() ([]byte, error) {
	return json.Marshal(tx)
}

func (tx *Transaction) Cost() *big.Int {
	if tx.Type() == NormalTx {
		gasCost := new(big.Int).Mul(tx.GasPrice.Price, new(big.Int).SetUint64(tx.GasLimit))
		return gasCost.Add(gasCost, tx.Value)
	}
	if tx.Type() == WithdrawTx {
		// withdraw Tx gets unique gas limit
		gasCost := new(big.Int).Mul(tx.GasPrice.Price, new(big.Int).SetUint64(tx.GasLimit))
		for _, outputCoin := range tx.OutputCoins {
			gasCost = gasCost.Add(gasCost, outputCoin.Amount)
		}
		return gasCost
	}
	if tx.Type() == RechargeTx {
		// Recharge Tx gets unique gas limit
		return new(big.Int).Mul(tx.GasPrice.Price, new(big.Int).SetUint64(tx.GasLimit))
	}
	return nil
}

func (tx *Transaction) Size() uint64 {
	ret, _ := tx.Serialize()
	return uint64(len(ret))
}

func (tx *Transaction) IntrinsicGas() (uint64, error) {
	if tx.Type() == NormalTx {
		// Set the starting gas for the raw transaction
		var gas uint64
		if (tx.To == common.Address{}) {
			gas = params.TxGasContractCreation
		} else {
			gas = params.TxGas
		}
		dataLen := uint64(len(tx.Data))
		// Bump the required gas by the amount of transactional data
		if dataLen > 0 {
			// Zero and non-zero bytes are priced differently
			var nz uint64
			for _, byt := range tx.Data {
				if byt != 0 {
					nz++
				}
			}
			// Make sure we don't exceed uint64 for all data combinations
			nonZeroGas := params.TxDataNonZeroGas

			if (math.MaxUint64-gas)/nonZeroGas < nz {
				return 0, ErrGasUintOverflow
			}
			gas += nz * nonZeroGas

			z := dataLen - nz
			if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
				return 0, ErrGasUintOverflow
			}
			gas += z * params.TxDataZeroGas

			if (tx.To == common.Address{}) {
				lenWords := toWordSize(dataLen)
				if (math.MaxUint64-gas)/params.InitCodeWordGas < lenWords {
					return 0, ErrGasUintOverflow
				}
				gas += lenWords * params.InitCodeWordGas
			}
		}
		if tx.AccessList != nil {
			gas += uint64(tx.AccessList.Len()) * params.TxAccessListAddressGas
			gas += uint64(tx.AccessList.StorageKeys()) * params.TxAccessListStorageKeyGas
		}
		return gas, nil
	}
	// other tx types are not determined yet
	return 0, nil
}

// toWordSize returns the ceiled word size required for init code payment calculation.
func toWordSize(size uint64) uint64 {
	if size > math.MaxUint64-31 {
		return math.MaxUint64/32 + 1
	}

	return (size + 31) / 32
}

func NewNormalTransaction(nonce uint64, to common.Address, value *big.Int, gasLimit uint64, gasPrice *gadget.GasPrice, data []byte, prv *ecdsa.PrivateKey) *Transaction {
	tx := &Transaction{
		TxPreface: TxPreface{
			From:     crypto.PubkeyToAddress(prv.PublicKey),
			Nonce:    nonce,
			Value:    value,
			GasLimit: gasLimit,
			GasPrice: gasPrice,
		},
		TxInner: TxInner{
			To:   to,
			Data: data,
		},
	}

	txBytes, _ := tx.Serialize()
	hash := common.GenerateHash(txBytes)
	var validate gadget.Validation
	validate.Sign(hash, prv)

	tx.TxHash = hash
	tx.Validation = &validate

	return tx
}

func NewWithdrawTransaction(nonce uint64, gasPrice *gadget.GasPrice, outputCoins []gadget.OutputCoin, prv *ecdsa.PrivateKey) *Transaction {
	tx := &Transaction{
		TxPreface: TxPreface{
			From:        crypto.PubkeyToAddress(prv.PublicKey),
			Nonce:       nonce,
			GasPrice:    gasPrice,
			OutputCoins: outputCoins,
		},
	}

	txBytes, _ := tx.Serialize()
	hash := common.GenerateHash(txBytes)
	var validate gadget.Validation
	validate.Sign(hash, prv)

	tx.TxHash = hash
	tx.Validation = &validate

	return tx
}

func NewRechargeTransaction(txHash common.Hash, inputCoins []gadget.InputCoin, witenesses []gadget.Witness, gasPrice *gadget.GasPrice, to common.Address) *Transaction {
	return &Transaction{
		TxPreface: TxPreface{
			TxHash:     txHash,
			InputCoins: inputCoins,
			Witnesses:  witenesses,
			GasPrice:   gasPrice,
		},
		TxInner: TxInner{
			To: to,
		},
	}
}

type Transactions []*Transaction

func (txs Transactions) Len() int { return len(txs) }

type TxByNonce Transactions

func (s TxByNonce) Len() int { return len(s) }
func (s TxByNonce) Less(i, j int) bool {
	return (s[i]).Nonce < (s[j]).Nonce
}
func (s TxByNonce) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// TxDifference returns a new set which is the difference between a and b.
func TxDifference(a, b Transactions) Transactions {
	keep := make(Transactions, 0, len(a))

	remove := make(map[common.Hash]struct{})
	for _, tx := range b {
		remove[tx.TxHash] = struct{}{}
	}

	for _, tx := range a {
		if _, ok := remove[tx.TxHash]; !ok {
			keep = append(keep, tx)
		}
	}

	return keep
}
