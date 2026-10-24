package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	ErrMalformedKey = errors.New("merchants: malformed API key")
	ErrUnknownMode  = errors.New("merchants: unknown mode")
)

type MerchantID [16]byte

func (id MerchantID) String() string {
	h := hex.EncodeToString(id[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Mode separates test keys, always served by simulators, from live keys (decision 33).
type Mode string

const (
	Test Mode = "test"
	Live Mode = "live"
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Test, Live:
		return m, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMode, s)
	}
}

const (
	lookupBytes = 8
	secretBytes = 32
)

var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var (
	lookupLen = encoding.EncodedLen(lookupBytes)
	secretLen = encoding.EncodedLen(secretBytes)
)

// NewKey is a freshly generated API key. Plaintext is shown to the merchant once and
// never stored; Lookup finds the stored row and Hash checks the rest.
type NewKey struct {
	Plaintext string
	Mode      Mode
	Lookup    string
	Hash      [sha256.Size]byte
}

// GenerateKey builds sk_<mode>_<lookup>_<secret>. The secret is 256 random bits, so a
// fast hash is enough: there is no guessable password to slow down an attacker on.
func GenerateKey(mode Mode, random io.Reader) (NewKey, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return NewKey{}, err
	}
	buf := make([]byte, lookupBytes+secretBytes)
	if _, err := io.ReadFull(random, buf); err != nil {
		return NewKey{}, fmt.Errorf("merchants: random key: %w", err)
	}
	lookup := encoding.EncodeToString(buf[:lookupBytes])
	plaintext := "sk_" + string(mode) + "_" + lookup + "_" + encoding.EncodeToString(buf[lookupBytes:])
	return NewKey{Plaintext: plaintext, Mode: mode, Lookup: lookup, Hash: sha256.Sum256([]byte(plaintext))}, nil
}

// ParseKey checks the shape of a presented key and returns its mode and lookup.
func ParseKey(plaintext string) (Mode, string, error) {
	parts := strings.Split(plaintext, "_")
	if len(parts) != 4 || parts[0] != "sk" || len(parts[2]) != lookupLen || len(parts[3]) != secretLen {
		return "", "", ErrMalformedKey
	}
	mode, err := ParseMode(parts[1])
	if err != nil {
		return "", "", ErrMalformedKey
	}
	for _, p := range parts[2:] {
		if _, err := encoding.DecodeString(p); err != nil {
			return "", "", ErrMalformedKey
		}
	}
	return mode, parts[2], nil
}

// Matches compares in constant time, so response timing does not reveal how many
// leading bytes of the hash a guess got right.
func Matches(plaintext string, storedHash []byte) bool {
	sum := sha256.Sum256([]byte(plaintext))
	return subtle.ConstantTimeCompare(sum[:], storedHash) == 1
}

// Principal is who an authenticated request acts as.
type Principal struct {
	MerchantID MerchantID
	Mode       Mode
}
