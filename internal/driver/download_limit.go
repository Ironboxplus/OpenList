package driver

// DownloadLimited is implemented by a driver whose storage refuses a second
// simultaneous download connection to one file (the drive answers 403, from any
// IP on the same account). OpenList then keeps its own background reads of such a
// file (subtitle extraction) out of the way of a playback of it: see
// stream.FileGate. A driver without it is not limited.
type DownloadLimited interface {
	// MaxDownloadConnectionsPerFile is how many connections may read one file at
	// once; 0 or less means no limit.
	MaxDownloadConnectionsPerFile() int
}

// DownloadConnectionsPerFile is the limit a storage declares, 0 when it has none.
func DownloadConnectionsPerFile(storage any) int {
	if l, ok := storage.(DownloadLimited); ok {
		return l.MaxDownloadConnectionsPerFile()
	}
	return 0
}
