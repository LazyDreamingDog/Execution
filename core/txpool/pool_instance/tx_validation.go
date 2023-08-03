package txpool_instance

import (
	"execution/common"
	"execution/core/state"
	"execution/core/types"
	"fmt"
	"math/big"
)

// ValidationOptions define certain differences between transaction validation
// across the different pools without having to duplicate those checks.
type ValidationOptions struct {
	MaxSize uint64   // Maximum size of a transaction that the caller can meaningfully handle
	MinTip  *big.Int // Minimum gas tip needed to allow a transaction into the caller pool
}

// ValidateTransaction is a helper method to check whether a transaction is valid
// according to the consensus rules, but does not check state-dependent validation
// (balance, nonce, etc).
//
// This check is public to allow different transaction pools to check the basic
// rules without duplicating code and running the risk of missed updates.
func ValidateTransaction(tx *types.Transaction, head *types.Header, opts *ValidationOptions) error {
	// Ensure transactions not implemented by the calling pool are rejected
	switch tx.Type() {
	case types.NormalTx, types.RechargeTx, types.WithdrawTx:
		break
	default:
		return fmt.Errorf("%w: tx type not supported by this pool", ErrTxTypeNotSupported)
	}

	if tx.Type() == types.NormalTx {
		// Before performing any expensive validations, sanity check that the tx is
		// smaller than the maximum limit the pool can meaningfully handle
		if tx.Size() > opts.MaxSize {
			return fmt.Errorf("%w: transaction size %v, limit %v", ErrOversizedData, tx.Size(), opts.MaxSize)
		}

		// Transactions can't be negative. This may never happen using RLP decoded
		// transactions but may occur for transactions created using the RPC.
		if tx.Value.Sign() < 0 {
			return ErrNegativeValue
		}
		// Ensure the transaction doesn't exceed the current block limit gas
		if (*head).GasLimit() < tx.GasLimit {
			return ErrGasLimit
		}
		// Sanity check for extremely large numbers (supported by RLP or RPC)
		if tx.GasPrice.Price.BitLen() > 256 {
			return ErrPriceVeryHigh
		}

		// Make sure the transaction is signed properly
		if _, err := tx.Validation.GetFrom(tx.TxHash); err != nil {
			return ErrInvalidSender
		}
		// Ensure the transaction has more gas than the bare minimum needed to cover
		// the transaction metadata
		intrGas, err := tx.IntrinsicGas()
		if err != nil {
			return err
		}
		if tx.GasLimit < intrGas {
			return fmt.Errorf("%w: needed %v, allowed %v", ErrIntrinsicGas, intrGas, tx.GasLimit)
		}
		if tx.GasPrice.Price.Cmp(opts.MinTip) < 0 {
			return fmt.Errorf("%w: tip needed %v, tip permitted %v", ErrUnderpriced, opts.MinTip, tx.GasPrice)
		}
	}

	return nil
}

// ValidationOptionsWithState define certain differences between stateful transaction
// validation across the different pools without having to duplicate those checks.
type ValidationOptionsWithState struct {
	State state.StateDB // State database to check nonces and balances against

	// FirstNonceGap is an optional callback to retrieve the first nonce gap in
	// the list of pooled transactions of a specific account. If this method is
	// set, nonce gaps will be checked and forbidden. If this method is not set,
	// nonce gaps will be ignored and permitted.
	FirstNonceGap func(addr common.Address) uint64

	// ExistingExpenditure is a mandatory callback to retrieve the cummulative
	// cost of the already pooled transactions to check for overdrafts.
	ExistingExpenditure func(addr common.Address, nonce uint64) *big.Int

	// ExistingCost is a mandatory callback to retrieve an already pooled
	// transaction's cost with the given nonce to check for overdrafts.
	ExistingCost func(addr common.Address, nonce uint64) *big.Int
}

// ValidateTransactionWithState is a helper method to check whether a transaction
// is valid according to the pool's internal state checks (balance, nonce, gaps).
//
// This check is public to allow different transaction pools to check the stateful
// rules without duplicating code and running the risk of missed updates.
func ValidateTransactionWithState(tx *types.Transaction, opts *ValidationOptionsWithState) error {
	if tx.Type() == types.NormalTx {
		// Ensure the transaction adheres to nonce ordering
		from := tx.From

		next := opts.State.GetNonce(from)
		if next > tx.Nonce {
			return fmt.Errorf("%w: next nonce %v, tx nonce %v", ErrNonceTooLow, next, tx.Nonce)
		}
		// Ensure the transaction doesn't produce a nonce gap in pools that do not
		// support arbitrary orderings
		if opts.FirstNonceGap != nil {
			if gap := opts.FirstNonceGap(from); gap < tx.Nonce {
				return fmt.Errorf("%w: tx nonce %v, gapped nonce %v", ErrNonceTooHigh, tx.Nonce, gap)
			}
		}
		// Ensure the transactor has enough funds to cover the transaction costs
		var (
			balance = opts.State.GetBalance(from) // this balance dose not include txCosts
			cost    = tx.Cost()
		)
		if balance.Cmp(cost) < 0 {
			return fmt.Errorf("%w: balance %v, tx cost %v, overshot %v", ErrInsufficientFunds, balance, cost, new(big.Int).Sub(cost, balance))
		}
		// Ensure the transactor has enough funds to cover for replacements or nonce
		// expansions without overdrafts
		// this spent only considers all txs ahead of this tx
		spent := opts.ExistingExpenditure(from, tx.Nonce)
		if prev := opts.ExistingCost(from, tx.Nonce); prev != nil {
			bump := new(big.Int).Sub(cost, prev)
			need := new(big.Int).Add(spent, bump)
			if balance.Cmp(need) < 0 {
				return fmt.Errorf("%w: balance %v, queued cost %v, tx bumped %v, overshot %v", ErrInsufficientFunds, balance, spent, bump, new(big.Int).Sub(need, balance))
			}
		} else {
			need := new(big.Int).Add(spent, cost)
			if balance.Cmp(need) < 0 {
				return fmt.Errorf("%w: balance %v, queued cost %v, tx cost %v, overshot %v", ErrInsufficientFunds, balance, spent, cost, new(big.Int).Sub(need, balance))
			}
		}
	}
	return nil
}
