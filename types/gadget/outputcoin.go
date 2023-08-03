package gadget

import (
	"execution/common"
	"math/big"
)

type OutputCoin struct {
	Amount *big.Int       `json:"amount"`
	Owner  common.Address `json:"owner"`
}
