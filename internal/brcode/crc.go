package brcode

// CRC16 is CRC-16/CCITT-FALSE, the one the manual names: polynomial 0x1021, initial
// value 0xFFFF, no reflection, no final XOR.
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
