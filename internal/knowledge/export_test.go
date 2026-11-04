package knowledge

// LowerArchiveLimits lowers the limits of an unpacked archive for a test and
// returns the function that restores them.
func LowerArchiveLimits(entries int, bytes, file, stream int64) (restore func()) {
	old := archiveLimits
	archiveLimits.entries, archiveLimits.bytes, archiveLimits.file, archiveLimits.stream = entries, bytes, file, stream
	return func() { archiveLimits = old }
}
