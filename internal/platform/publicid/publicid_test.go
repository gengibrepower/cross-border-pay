package publicid

import (
	"bytes"
	"sort"
	"testing"
)

func TestParseReadsWhatEncodeWrote(t *testing.T) {
	id := [16]byte{0x01, 0x9a, 0x3b, 0xff, 0x00, 0x10}
	s := Encode("evt_", id)
	if len(s) != len("evt_")+26 {
		t.Fatalf("Encode() = %q, want 26 characters after the prefix", s)
	}
	got, ok := Parse("evt_", s)
	if !ok || got != id {
		t.Errorf("Parse(%q) = %x, %v; want %x", s, got, ok, id)
	}
}

func TestParseRefuses(t *testing.T) {
	good := Encode("evt_", [16]byte{1})
	for _, s := range []string{
		"", "evt_", good[4:], "pay_" + good[4:], good + "0", good[:len(good)-1],
		"evt_" + "uuuuuuuuuuuuuuuuuuuuuuuuuu", // u is not in the alphabet
		"EVT_" + good[4:],
	} {
		if _, ok := Parse("evt_", s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

// Decision 37: the IDs sort as strings in the order of their bytes, so a UUIDv7's
// creation order survives.
func TestEncodedIDsSortLikeTheirBytes(t *testing.T) {
	ids := [][16]byte{{0xff}, {0x00, 0x01}, {0x80}, {0x00, 0x00, 0x01}, {0x7f, 0xff}}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = Encode("pay_", id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	sort.Strings(strs)
	for i := range ids {
		if strs[i] != Encode("pay_", ids[i]) {
			t.Fatalf("string order differs from byte order at %d", i)
		}
	}
}
