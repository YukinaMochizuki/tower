package auth

import "unicode/utf8"

// ASCII reports whether s is all ASCII. Emails that name people must be:
// lower-casing some other characters makes an ASCII letter (U+212A KELVIN
// SIGN becomes k), so a non-ASCII address could fold into someone else's.
func ASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
