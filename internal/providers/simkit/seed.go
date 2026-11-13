package simkit

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
)

// Seed is the one number every random choice of a simulator comes from.
type Seed uint64

// RNG returns a generator for key alone. Seeding by the key, not by arrival, is what
// gives the same answers whatever order concurrent requests come in.
func (s Seed) RNG(key string) *rand.Rand {
	sum := sha256.Sum256([]byte(key))
	return rand.New(rand.NewPCG(uint64(s), binary.BigEndian.Uint64(sum[:8]))) //nolint:gosec // G404: determinism from the seed is the point; nothing here is a secret.
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// Text draws n characters of digits and capital letters.
func Text(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}
