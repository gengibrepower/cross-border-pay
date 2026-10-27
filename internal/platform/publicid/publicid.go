// Package publicid writes and reads the public IDs of decision 37: a type prefix and
// the UUIDv7 in Crockford base32, whose alphabet is in ASCII order, so IDs sort by
// creation time as strings too. Every module that shows or reads an ID uses this one
// encoding, so a pay_ or evt_ means the same bytes everywhere.
package publicid

import (
	"encoding/base32"
	"strings"
)

var encoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func Encode(prefix string, id [16]byte) string {
	return prefix + encoding.EncodeToString(id[:])
}

// Parse reads an ID written by Encode with the same prefix.
func Parse(prefix, s string) ([16]byte, bool) {
	encoded, ok := strings.CutPrefix(s, prefix)
	if !ok || len(encoded) != encoding.EncodedLen(16) {
		return [16]byte{}, false
	}
	b, err := encoding.DecodeString(encoded)
	if err != nil {
		return [16]byte{}, false
	}
	return [16]byte(b), true
}
