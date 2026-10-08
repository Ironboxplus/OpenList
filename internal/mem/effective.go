package mem

import "github.com/shirou/gopsutil/v4/mem"

// EffectiveMemory is the host's memory, narrowed to the container's cgroup
// limit when there is one: inside a container /proc/meminfo still reports
// the whole host.
func EffectiveMemory() (total, available uint64, err error) {
	m, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	total, available = m.Total, m.Available
	if c := containerLimit(); c != nil {
		total, available = c.clamp(total, available)
	}
	return total, available, nil
}
