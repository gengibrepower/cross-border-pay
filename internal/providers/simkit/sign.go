package simkit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureTolerance bounds how old (or how far ahead) a signature may be, so a
// captured request cannot be replayed later.
const SignatureTolerance = 5 * time.Minute

var (
	ErrBadSignature   = errors.New("simulator: event signature does not match")
	ErrStaleSignature = errors.New("simulator: event signature is too old or from the future")
)

// Sign returns the signature header for body sent at t: the HMAC-SHA256 of the Unix
// time, a dot and the body. Signing the time with the body is what makes the
// tolerance check mean anything.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

func mac(secret []byte, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks the signature header against body. Any v1 may match, so the sender
// can sign with an old and a new secret while rotating.
func Verify(secret []byte, header string, body []byte, now time.Time) error {
	var ts string
	var sigs [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return fmt.Errorf("%w: header %q", ErrBadSignature, header)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return fmt.Errorf("%w: signed %v away from now", ErrStaleSignature, d)
	}
	want := mac(secret, ts, body)
	matched := false
	for _, sig := range sigs {
		// Constant time: comparing byte by byte and stopping at the first difference
		// would tell an attacker, through timing, how much of a forgery is right.
		matched = hmac.Equal(sig, want) || matched
	}
	if !matched {
		return ErrBadSignature
	}
	return nil
}
