package mem

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCg(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const gib = 1 << 30

func TestCgroupV2Limited(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "memory.max", "2147483648\n")
	writeCg(t, root, "memory.current", "1073741824\n")
	writeCg(t, root, "memory.stat", "anon 100\ninactive_file 268435456\nactive_file 5\n")
	c := detectCgroup(root, "0::/\n")
	if c == nil || c.limit != 2*gib {
		t.Fatalf("detect = %+v", c)
	}
	// 1 GiB current minus 256 MiB of reclaimable cache.
	total, avail := c.clamp(515*gib, 462*gib)
	if total != 2*gib || avail != 2*gib-(gib-gib/4) {
		t.Fatalf("clamp = %d, %d", total, avail)
	}
	// A host with less than the limit keeps its own numbers.
	total, avail = c.clamp(gib, gib/2)
	if total != gib || avail != gib/2 {
		t.Fatalf("clamp small host = %d, %d", total, avail)
	}
}

func TestCgroupV2OwnPath(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "job/memory.max", "1073741824")
	writeCg(t, root, "job/memory.current", "0")
	c := detectCgroup(root, "0::/job\n")
	if c == nil || c.limit != gib {
		t.Fatalf("detect = %+v", c)
	}
	if _, avail := c.clamp(8*gib, 8*gib); avail != gib {
		t.Fatalf("avail = %d", avail)
	}
}

func TestCgroupV2Unlimited(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "memory.max", "max\n")
	writeCg(t, root, "memory.current", "12345\n")
	if c := detectCgroup(root, "0::/\n"); c != nil {
		t.Fatalf("detect = %+v, want none", c)
	}
}

func TestCgroupV1(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "memory/memory.limit_in_bytes", "536870912\n")
	writeCg(t, root, "memory/memory.usage_in_bytes", "402653184\n")
	writeCg(t, root, "memory/memory.stat", "cache 1\ntotal_inactive_file 134217728\n")
	c := detectCgroup(root, "12:cpu,cpuacct:/\n4:memory:/\n")
	if c == nil || c.limit != gib/2 {
		t.Fatalf("detect = %+v", c)
	}
	// 384 MiB used minus 128 MiB cache leaves 256 MiB.
	if total, avail := c.clamp(64*gib, 60*gib); total != gib/2 || avail != gib/4 {
		t.Fatalf("clamp = %d, %d", total, avail)
	}
}

func TestCgroupV1Unlimited(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "memory/memory.limit_in_bytes", "9223372036854771712\n")
	writeCg(t, root, "memory/memory.usage_in_bytes", "1\n")
	if c := detectCgroup(root, "4:memory:/\n"); c != nil {
		t.Fatalf("detect = %+v, want none", c)
	}
}

func TestCgroupAbsent(t *testing.T) {
	if c := detectCgroup(t.TempDir(), "0::/\n"); c != nil {
		t.Fatalf("detect = %+v, want none", c)
	}
	if c := detectCgroup(t.TempDir(), ""); c != nil {
		t.Fatalf("detect = %+v, want none", c)
	}
}

func TestCgroupUsageOverLimitClampsToZero(t *testing.T) {
	root := t.TempDir()
	writeCg(t, root, "memory.max", "1000")
	writeCg(t, root, "memory.current", "5000")
	c := detectCgroup(root, "0::/\n")
	if _, avail := c.clamp(1<<40, 1<<40); avail != 0 {
		t.Fatalf("avail = %d", avail)
	}
}

func TestEffectiveMemory(t *testing.T) {
	total, avail, err := EffectiveMemory()
	if err != nil || total == 0 || avail > total {
		t.Fatalf("total=%d avail=%d err=%v", total, avail, err)
	}
}
