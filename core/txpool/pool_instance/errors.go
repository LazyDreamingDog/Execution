package txpool_instance

import "errors"

var (
	ErrAlreadyKnown         = errors.New("transaction already known")
	ErrUnderpriced          = errors.New("transaction underpriced")
	ErrTxPoolOverflow       = errors.New("transaction pool overflow")
	ErrNonceTooLow          = errors.New("nonce too low")
	ErrNonceTooHigh         = errors.New("nonce too high")
	ErrInsufficientFunds    = errors.New("insufficient funds")
	ErrFutureReplacePending = errors.New("future replace pending")
	ErrReplaceUnderpriced   = errors.New("replace transaction underpriced")
	ErrTxTypeNotSupported   = errors.New("transaction type not supported")
	ErrOversizedData        = errors.New("transaction data too big")
	ErrNegativeValue        = errors.New("negative value")
	ErrGasLimit             = errors.New("gas limit too high")
	ErrPriceVeryHigh        = errors.New("gas price too high")
	ErrInvalidSender        = errors.New("invalid sender")
	ErrIntrinsicGas         = errors.New("intrinsic gas too low")
)
