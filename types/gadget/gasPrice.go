package gadget

import "math/big"

type GasPrice struct {
	Price *big.Int `json:"price,omitempty"`
}

func NewGasPrice(price *big.Int) *GasPrice {
	return &GasPrice{Price: price}
}
