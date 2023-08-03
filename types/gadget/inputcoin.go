package gadget

import (
	"execution/common"
	"math/big"
)

type InputCoin struct {
	TxHash common.Hash `json:"txHash"`
	Index  uint32      `json:"index"`
	Amount *big.Int    `json:"amount"`

	WitnessIndex uint32 `json:"witnessIndex"`
	Owner        []byte `json:"owner"`
}
