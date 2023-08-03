package types

import (
	"execution/common"
	"math/big"
)

type Header struct {
	hash       common.Hash
	parentHash common.Hash
	number     *big.Int
	gasLimit   uint64
}

func NewHeader(hash common.Hash, parentHash common.Hash, number *big.Int, gasLimit uint64) *Header {
	return &Header{
		hash:       hash,
		parentHash: parentHash,
		number:     number,
		gasLimit:   gasLimit,
	}
}

func (header *Header) Hash() common.Hash {
	return header.hash
}

func (header *Header) ParentHash() common.Hash {
	return header.parentHash
}

func (header *Header) Number() *big.Int {
	return header.number
}

func (header *Header) GasLimit() uint64 {
	return header.gasLimit
}

type Body struct {
	transactions Transactions
}

func NewBody(transactions Transactions) *Body {
	return &Body{
		transactions: transactions,
	}
}

func (body *Body) Transactions() Transactions {
	return body.transactions
}

type Block struct {
	header *Header
	body   *Body
}

func NewBlock(header *Header, body *Body) *Block {
	return &Block{
		header: header,
		body:   body,
	}
}

func (block *Block) Header() *Header {
	return block.header
}

func (block *Block) Hash() common.Hash {
	return block.header.Hash()
}

func (block *Block) ParentHash() common.Hash {
	return block.header.ParentHash()
}

func (block *Block) NumberU64() uint64 {
	return block.header.Number().Uint64()
}

func (block *Block) Transactions() Transactions {
	return block.body.Transactions()
}
