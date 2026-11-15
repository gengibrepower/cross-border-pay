// Package publicid is decision 37: Crockford base32's alphabet is in ASCII order, so
// IDs sort by creation time as strings too.
package publicid

import (
	"encoding/base32"
	"strings"
)

var encoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func Encode(prefix string, id [16]byte) string {
	return prefix + encoding.EncodeToString(id[:])
}

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
