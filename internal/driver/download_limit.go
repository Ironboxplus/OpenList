package driver

// DownloadLimited is implemented by a driver whose storage refuses more than a
// few simultaneous download connections to one file (the drive answers 403, from
// any IP on the same account). OpenList then keeps its own background reads of
// such a file (subtitle extraction) within that number, and gives playback the
// slots first: see stream.FileGate. A driver without it is not limited.
type DownloadLimited interface {
	// MaxDownloadConnectionsPerFile is how many connections may read one file at
	// once; 0 or less means no limit.
	MaxDownloadConnectionsPerFile() int
}

// DefaultConnectionsPerFile is the limit of a storage that has the setting and
// was saved before it existed.
const DefaultConnectionsPerFile = 2

// ConnectionsSetting reads a max_conns_per_file setting kept as a pointer, so
// that "not set" (the default) can be told from 0 (unlimited).
func ConnectionsSetting(set *int) int {
	if set == nil {
		return DefaultConnectionsPerFile
	}
	return *set
}

// DownloadConnectionsPerFile is the limit a storage declares, 0 when it has none.
func DownloadConnectionsPerFile(storage any) int {
	if l, ok := storage.(DownloadLimited); ok {
		return l.MaxDownloadConnectionsPerFile()
	}
	return 0
}
