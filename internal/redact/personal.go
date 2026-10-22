package redact

import (
	"regexp"
	"strings"
	"time"
)

// Personal data in free text: agent memory, offloaded conversation turns,
// review comments and prompts. Unlike secrets, these patterns collide with
// ordinary numbers (timestamps, versions, line numbers, IDs), so every
// candidate is validated (Luhn and issuer prefix for cards, a real date for
// national identity numbers, a separator layout for phone numbers) and only
// formatted numbers are considered. Undelimited digit runs are left alone.

// Placeholders name what was removed, so a model still reads the sentence.
const (
	CardPlaceholder       = "[CARD]"
	NationalIDPlaceholder = "[NATIONAL-ID]"
	PhonePlaceholder      = "[PHONE]"
)

var (
	// 13 to 19 digits, unbroken or in the printed groups (4-4-4-x, Amex
	// 4-6-5), so a number written after a card is not swallowed into it.
	cardRe = regexp.MustCompile(`\b(?:\d{13,19}|\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{1,7}|\d{4}[ -]\d{6}[ -]\d{5})\b`)
	// US social security numbers, always dashed: 123-45-6789.
	ssnRe = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	// Nordic identity numbers: Danish CPR and Norwegian/Finnish style
	// DDMMYY-NNNN, Swedish YYMMDD-NNNN and YYYYMMDD-NNNN (also with +).
	nordicRe = regexp.MustCompile(`\b(\d{6}|\d{8})[-+](\d{4})\b`)
	// International numbers: +CC then more digits in groups. The plus must
	// start the text or follow a space or punctuation, not a word or a
	// version (semver build metadata is 1.2.3+20240101).
	intlPhoneRe = regexp.MustCompile(`(?:^|[\s(:;,"'])\+\d{1,3}(?:[ .-]?\(?\d{1,4}\)?){2,6}\b`)
	// North American (555) 123-4567 and 555-123-4567.
	naPhoneRe = regexp.MustCompile(`(?:\(\d{3}\)\s?|\b\d{3}-)\d{3}-\d{4}\b`)
	// Danish and other 8-digit numbers written in pairs: 12 34 56 78.
	// They never start with 0 or 1, which keeps byte dumps out.
	pairPhoneRe = regexp.MustCompile(`\b[2-9]\d \d{2} \d{2} \d{2}\b`)
)

// Personal returns s with payment card numbers, national identity numbers
// and phone numbers replaced by placeholders.
func Personal(s string) string {
	if !strings.ContainsAny(s, "0123456789") {
		return s
	}
	s = cardRe.ReplaceAllStringFunc(s, func(m string) string {
		if isCard(digitsOf(m)) {
			return CardPlaceholder
		}
		return m
	})
	s = ssnRe.ReplaceAllStringFunc(s, func(m string) string {
		// Area 000, 666 and 9xx, group 00 and serial 0000 are never issued.
		if m[:3] == "000" || m[:3] == "666" || m[0] == '9' || m[4:6] == "00" || m[7:] == "0000" {
			return m
		}
		return NationalIDPlaceholder
	})
	s = nordicRe.ReplaceAllStringFunc(s, func(m string) string {
		if isBirthDate(m[:strings.IndexAny(m, "-+")]) {
			return NationalIDPlaceholder
		}
		return m
	})
	s = intlPhoneRe.ReplaceAllStringFunc(s, func(m string) string {
		if n := len(digitsOf(m)); n >= 8 && n <= 15 {
			if m[0] != '+' {
				return m[:1] + PhonePlaceholder // keep the separator before it
			}
			return PhonePlaceholder
		}
		return m
	})
	s = naPhoneRe.ReplaceAllString(s, PhonePlaceholder)
	return pairPhoneRe.ReplaceAllString(s, PhonePlaceholder)
}

// Free prepares free text written by people or agents for storage or a
// model: secrets first, then personal data.
func Free(s string) string { return Personal(Text(s)) }

func digitsOf(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// isCard checks the issuer prefix and length of the major networks and the
// Luhn checksum. Millisecond timestamps (1…) and most IDs fail the prefix.
func isCard(d string) bool {
	n := len(d)
	ok := false
	switch {
	case d[0] == '4': // Visa
		ok = n == 13 || n == 16 || n == 19
	case d[:2] >= "51" && d[:2] <= "55", d[:4] >= "2221" && d[:4] <= "2720": // Mastercard
		ok = n == 16
	case d[:2] == "34" || d[:2] == "37": // American Express
		ok = n == 15
	case d[:4] == "6011" || d[:2] == "65", d[:3] >= "644" && d[:3] <= "649": // Discover
		ok = n >= 16 && n <= 19
	case d[:4] >= "3528" && d[:4] <= "3589": // JCB
		ok = n >= 16 && n <= 19
	}
	return ok && luhn(d)
}

func luhn(d string) bool {
	sum := 0
	for i := range len(d) {
		x := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			if x *= 2; x > 9 {
				x -= 9
			}
		}
		sum += x
	}
	return sum%10 == 0
}

// isBirthDate accepts DDMMYY and YYMMDD (six digits) or YYYYMMDD (eight)
// that name a real date; the eight-digit form also not in the future, which
// keeps most date-stamped build IDs out.
func isBirthDate(d string) bool {
	valid := func(layout, v string) bool { _, err := time.Parse(layout, v); return err == nil }
	if len(d) == 8 {
		t, err := time.Parse("20060102", d)
		return err == nil && t.Year() >= 1900 && !t.After(time.Now())
	}
	return valid("020106", d) || valid("060102", d)
}
