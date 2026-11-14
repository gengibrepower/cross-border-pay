package boleto

// mod10 is the check digit of each field of the line: from the right, digits weigh 2,
// 1, 2, ...; a product above 9 counts as the sum of its digits; the digit is what takes
// the total to the next multiple of 10.
func mod10(s string) byte {
	sum := 0
	weight := 2
	for i := len(s) - 1; i >= 0; i-- {
		p := int(s[i]-'0') * weight
		sum += p/10 + p%10
		weight = 3 - weight
	}
	return byte('0' + (10-sum%10)%10)
}

// mod11 is the general check digit of the barcode, over its other 43 digits: from the
// right, digits weigh 2 to 9 and back to 2; the digit is 11 minus the remainder by 11,
// and 1 when that gives 0, 10 or 11, since a 0 would not be read.
func mod11(s string) byte {
	sum := 0
	weight := 2
	for i := len(s) - 1; i >= 0; i-- {
		sum += int(s[i]-'0') * weight
		weight++
		if weight > 9 {
			weight = 2
		}
	}
	d := 11 - sum%11
	if d == 0 || d == 10 || d == 11 {
		return '1'
	}
	return byte('0' + d)
}
