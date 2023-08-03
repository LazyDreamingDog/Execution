package types

import "errors"

var (
	ErrGasUintOverflow = errors.New("gas uint overflow")
	ErrCannotMarshal   = errors.New("cannot marshal")
)
