package brcode

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse checks the CRC before anything else, then reads the fields. Fields it does not
// need, such as other arrangements' templates or recurrence (80), are skipped.
func Parse(payload string) (Code, error) {
	if !printableASCII(payload) {
		return Code{}, fmt.Errorf("%w: not printable ASCII", ErrMalformed)
	}
	n := len(payload)
	if n < 8 || payload[n-8:n-4] != idCRC+"04" {
		return Code{}, fmt.Errorf("%w: does not end with the CRC field 6304", ErrMalformed)
	}
	want, err := strconv.ParseUint(payload[n-4:], 16, 16)
	if err != nil {
		return Code{}, fmt.Errorf("%w: CRC %q is not hexadecimal", ErrMalformed, payload[n-4:])
	}
	if got := CRC16([]byte(payload[:n-4])); uint64(got) != want {
		return Code{}, fmt.Errorf("%w: computed %04X, payload says %s", ErrInvalidCRC, got, payload[n-4:])
	}

	fields, err := parseTLV(payload[:n-8])
	if err != nil {
		return Code{}, err
	}
	if !strings.HasPrefix(payload, idPayloadFormat+"02"+payloadFormatVersion) {
		return Code{}, fmt.Errorf("%w: must start with payload format indicator 01", ErrMalformed)
	}
	if _, ok := fields[idCRC]; ok {
		return Code{}, fmt.Errorf("%w: CRC field before the end", ErrMalformed)
	}
	if v, ok := fields[idPointOfInitiation]; ok && v != reusable && v != singleUse {
		return Code{}, fmt.Errorf("%w: point of initiation %q", ErrInvalidField, v)
	}
	if fields[idCurrency] != currencyBRL || fields[idCountry] != countryBR {
		return Code{}, fmt.Errorf("%w: currency %q and country %q, want 986 and BR", ErrInvalidField, fields[idCurrency], fields[idCountry])
	}
	if v := fields[idMerchantCategory]; len(v) != 4 || !digits(v) {
		return Code{}, fmt.Errorf("%w: merchant category code %q", ErrInvalidField, v)
	}

	account, err := pixAccount(fields)
	if err != nil {
		return Code{}, err
	}
	c := Code{
		Key:          account[subKey],
		URL:          account[subURL],
		Info:         account[subInfo],
		MerchantName: fields[idMerchantName],
		MerchantCity: fields[idMerchantCity],
	}
	if v, ok := fields[idAmount]; ok {
		if c.Amount, err = parseAmount(v); err != nil {
			return Code{}, err
		}
	}
	additional, err := parseTLV(fields[idAdditionalData])
	if err != nil {
		return Code{}, fmt.Errorf("additional data: %w", err)
	}
	txid, ok := additional[subTxID]
	if !ok {
		return Code{}, fmt.Errorf("%w: no txid (62-05); it is always present, as *** if unused", ErrMalformed)
	}
	if txid != noTxID {
		c.TxID = txid
	}
	if err := c.validate(); err != nil {
		return Code{}, err
	}
	return c, nil
}

// pixAccount finds the one Merchant Account Information template (IDs 26 to 51) of
// the PIX arrangement; templates of other arrangements may sit beside it.
func pixAccount(fields map[string]string) (map[string]string, error) {
	var found map[string]string
	for id := 26; id <= 51; id++ {
		v, ok := fields[strconv.Itoa(id)]
		if !ok {
			continue
		}
		if len(v) > maxTemplate {
			return nil, fmt.Errorf("%w: template %d has %d characters, at most %d", ErrFieldTooLong, id, len(v), maxTemplate)
		}
		sub, err := parseTLV(v)
		if err != nil {
			return nil, fmt.Errorf("template %d: %w", id, err)
		}
		if !strings.EqualFold(sub[subGUI], GUI) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: more than one %s template", ErrMalformed, GUI)
		}
		found = sub
	}
	if found == nil {
		return nil, fmt.Errorf("%w: no %s template", ErrMalformed, GUI)
	}
	return found, nil
}

// parseTLV reads a sequence of ID (2 digits), length (2 digits) and value.
func parseTLV(s string) (map[string]string, error) {
	fields := map[string]string{}
	for s != "" {
		if len(s) < 4 || !digits(s[:4]) {
			return nil, fmt.Errorf("%w: bad field header %q", ErrMalformed, s[:min(len(s), 4)])
		}
		id := s[:2]
		n, _ := strconv.Atoi(s[2:4])
		if len(s) < 4+n {
			return nil, fmt.Errorf("%w: field %s says %d characters, %d remain", ErrMalformed, id, n, len(s)-4)
		}
		if _, dup := fields[id]; dup {
			return nil, fmt.Errorf("%w: field %s appears twice", ErrMalformed, id)
		}
		fields[id] = s[4 : 4+n]
		s = s[4+n:]
	}
	return fields, nil
}
