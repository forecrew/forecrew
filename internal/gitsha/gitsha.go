// Package gitsha tells commit IDs from branch names: a ref that is 40 (SHA-1)
// or 64 (SHA-256) lowercase hex digits is a full commit ID, 7 to 39 digits
// may be an abbreviated one.
package gitsha

// IsFull reports whether s is a full commit ID: 40 or 64 lowercase hex
// digits.
func IsFull(s string) bool {
	return (len(s) == 40 || len(s) == 64) && hex(s)
}

// IsShort reports whether s is 7 to 39 lowercase hex digits, which is an
// abbreviated commit ID or a branch that looks like one.
func IsShort(s string) bool {
	return len(s) >= 7 && len(s) <= 39 && hex(s)
}

// LooksLikeCommit reports whether s is a full or an abbreviated commit ID.
func LooksLikeCommit(s string) bool { return IsFull(s) || IsShort(s) }

func hex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
