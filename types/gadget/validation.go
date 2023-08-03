package gadget

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"execution/common"
	"math/big"

	"execution/crypto" // substitude to our crypto
)

var (
	ErrInvalidSignature = errors.New("invalid signature")
	ErrInvalidPubKey    = errors.New("invalid public key")
)

type Validation struct {
	R *big.Int `json:"r,omitempty"`
	S *big.Int `json:"s,omitempty"`
	V *big.Int `json:"v,omitempty"`
}

func validateSignatureValues(r, s *big.Int, v byte) bool {
	Big1 := big.NewInt(1)
	if r.Cmp(Big1) < 0 || s.Cmp(Big1) < 0 || s.Cmp(crypto.Secp256k1halfN) > 0 {
		return false
	}
	return r.Cmp(crypto.Secp256k1N) < 0 && s.Cmp(crypto.Secp256k1N) < 0 && (v == 0 || v == 1)
}

func (sign *Validation) GetFrom(input common.Hash) (common.Address, error) {
	if sign.V.BitLen() > 8 {
		return common.Address{}, ErrInvalidSignature
	}
	v := byte(sign.V.Uint64() - 27)

	if !validateSignatureValues(sign.R, sign.S, v) {
		return common.Address{}, ErrInvalidSignature
	}

	sig := make([]byte, 65)
	r := sign.R.Bytes()
	s := sign.S.Bytes()
	copy(sig[32-len(r):32], r)
	copy(sig[64-len(s):64], s)
	sig[64] = v

	pub, err := crypto.Ecrecover(input[:], sig)
	if err != nil {
		return common.Address{}, err
	}
	if len(pub) == 0 || pub[0] != 4 {
		return common.Address{}, ErrInvalidPubKey
	}

	var addr common.Address
	copy(addr[:], crypto.Keccak256(pub[1:])[12:])

	return addr, nil
}

func (sign *Validation) Sign(input common.Hash, prv *ecdsa.PrivateKey) {
	sig, err := crypto.Sign(input[:], prv)
	if err != nil {
		panic(err)
	}
	sign.R = new(big.Int).SetBytes(sig[:32])
	sign.S = new(big.Int).SetBytes(sig[32:64])
	sign.V = new(big.Int).SetBytes([]byte{sig[64] + 27})
}

func FromECDSAPub(pub *ecdsa.PublicKey) []byte {
	if pub == nil || pub.X == nil || pub.Y == nil {
		return nil
	}
	return elliptic.Marshal(crypto.S256(), pub.X, pub.Y)
}

func PubkeyToAddress(p ecdsa.PublicKey) common.Address {
	pubBytes := FromECDSAPub(&p)
	from := common.Address{}
	from.SetBytes(crypto.Keccak256(pubBytes[1:])[12:])
	return from
}
