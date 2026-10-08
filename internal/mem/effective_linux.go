//go:build linux

package mem

import (
	"os"
	"sync"
)

var (
	cgroupOnce sync.Once
	cgroupMem  *cgroupLimit
)

// containerLimit is the process's cgroup memory limit, looked up once.
func containerLimit() *cgroupLimit {
	cgroupOnce.Do(func() {
		b, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return
		}
		cgroupMem = detectCgroup("/sys/fs/cgroup", string(b))
	})
	return cgroupMem
}
