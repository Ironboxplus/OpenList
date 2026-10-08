package mem

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupLimit is the memory limit of a cgroup and where to read its usage.
// Parsing lives here, free of build tags, so it can be tested with a fake
// /sys/fs/cgroup; only the Linux wiring is in effective_linux.go.
type cgroupLimit struct {
	limit     uint64
	usageFile string
	statFile  string
	// inactiveKey is the memory.stat key of the page cache that can be
	// dropped, which does not count as used.
	inactiveKey string
}

// v1Unlimited is the threshold above which a v1 limit means "none"; the kernel
// reports a page-rounded MaxInt64 there.
const v1Unlimited = 1 << 60

// detectCgroup finds the memory limit that applies to a process whose
// /proc/self/cgroup reads procCgroup, under the cgroup mount at root. It
// returns nil when there is no finite limit.
func detectCgroup(root, procCgroup string) *cgroupLimit {
	var v2Path, v1Path string
	for _, line := range strings.Split(procCgroup, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2Path = parts[2]
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == "memory" {
				v1Path = parts[2]
			}
		}
	}
	// A container normally sees itself at "/", but the process may sit in a
	// sub-cgroup of the one that is limited, or the path may not be mounted:
	// try its own directory, then the mount point.
	for _, dir := range []string{filepath.Join(root, v2Path), root} {
		if l := readLimit(filepath.Join(dir, "memory.max"), filepath.Join(dir, "memory.current"),
			filepath.Join(dir, "memory.stat"), "inactive_file"); l != nil {
			return l
		}
	}
	base := filepath.Join(root, "memory")
	for _, dir := range []string{filepath.Join(base, v1Path), base} {
		if l := readLimit(filepath.Join(dir, "memory.limit_in_bytes"), filepath.Join(dir, "memory.usage_in_bytes"),
			filepath.Join(dir, "memory.stat"), "total_inactive_file"); l != nil {
			return l
		}
	}
	return nil
}

func readLimit(limitFile, usageFile, statFile, inactiveKey string) *cgroupLimit {
	b, err := os.ReadFile(limitFile)
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 || n >= v1Unlimited {
		return nil
	}
	return &cgroupLimit{limit: n, usageFile: usageFile, statFile: statFile, inactiveKey: inactiveKey}
}

// used is the current usage without the reclaimable page cache.
func (c *cgroupLimit) used() (uint64, error) {
	b, err := os.ReadFile(c.usageFile)
	if err != nil {
		return 0, err
	}
	u, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	if st, err := os.ReadFile(c.statFile); err == nil {
		for _, line := range strings.Split(string(st), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || k != c.inactiveKey {
				continue
			}
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				u -= min(u, n)
			}
			break
		}
	}
	return u, nil
}

// clamp narrows the host's numbers to what the cgroup allows.
func (c *cgroupLimit) clamp(total, available uint64) (uint64, uint64) {
	total = min(total, c.limit)
	used, err := c.used()
	if err != nil {
		return total, min(available, c.limit)
	}
	return total, min(available, c.limit-min(used, c.limit))
}
