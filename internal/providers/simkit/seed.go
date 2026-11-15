package simkit

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
)

type Seed uint64

// RNG seeds by the key, not by arrival, so concurrent requests get the same answers
// in any order.
func (s Seed) RNG(key string) *rand.Rand {
	sum := sha256.Sum256([]byte(key))
	return rand.New(rand.NewPCG(uint64(s), binary.BigEndian.Uint64(sum[:8]))) //nolint:gosec // G404: determinism from the seed is the point; nothing here is a secret.
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func Text(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}
