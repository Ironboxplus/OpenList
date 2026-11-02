//go:build blackbox

package blackbox

import (
	"testing"
	"time"
)

// rotateWhileDown builds a 3-node cluster, stops n3, lets the refresher rotate
// the pair (so n3's stored pair is dead on the provider side), waits until the
// surviving members hold the new pair, and returns the cluster.
func rotateWhileDown(t *testing.T, name string) (c *cluster, n3 *node) {
	t.Helper()
	const ttl = 20 * time.Second
	c = buildCluster(t, name, ttl, fastTiming, newBin(t), true, 2)
	n1, n2 := c.nodes[0], c.nodes[1]
	n3 = c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)

	n3.stop()
	before, _ := n2.group(c.groupID)
	c.emu.ExpireAccess()
	if err := n2.list(); err != nil {
		t.Fatalf("refresher should recover transparently: %v", err)
	}
	waitUntil(t, "the surviving members to hold the rotated pair", 30*time.Second, func() bool {
		g1, ok1 := n1.group(c.groupID)
		g2, ok2 := n2.group(c.groupID)
		return ok1 && ok2 && g2.Version > before.Version && g1.Version == g2.Version
	})
	return c, n3
}

// BB6 (T3): a member is stopped while the pair rotates. When it comes back its
// stored pair is dead; the group's current pair is already in the cluster and
// must be applied without anyone calling the provider's refresh again.
func TestBB6_StoppedMemberRestartsAfterRotationConvergesWithoutRefresh(t *testing.T) {
	c, n3 := rotateWhileDown(t, "s6-rotate-while-down")
	c.emu.ResetStats()
	n3 = c.restart(n3)
	t0 := time.Now()
	waitUntil(t, "the restarted member to serve requests", 60*time.Second, func() bool { return n3.list() == nil })
	record(t, "restarted n3 served requests %s after start", time.Since(t0).Round(100*time.Millisecond))
	st := c.emu.Stats()
	if st.RefreshCalls != 0 {
		t.Fatalf("recovery called the provider's refresh %d time(s): %+v", st.RefreshCalls, st)
	}
	if n3.refreshOK() != 0 {
		t.Fatalf("restarted member spent a refresh_token")
	}
	waitUntil(t, "everyone converged", 30*time.Second, c.converged)
}

// BB7 (T1): the node starts with an expired pair, the cluster already holds the
// newer valid one. The storage's Init gets a 401 before it is registered, which
// is when the old event-driven recovery lost its only chance. Recovery must
// happen within a few resync periods.
func TestBB7_StartWithExpiredPairAdoptsHeldRecordWithinResync(t *testing.T) {
	c, n3 := rotateWhileDown(t, "s7-expired-start")
	c.emu.ResetStats()
	// Init of the restarted node's storage is slowed to 8 s: the cluster hands
	// over the held record while the storage is still not registered.
	slow := newSlowEmu(t, c.emuSrv.URL, 8*time.Second)
	n3 = c.restartWith(n3, slow.srv.URL)
	t0 := time.Now()
	slow.delay.Store(0)
	bound := 4*time.Duration(fastTiming.Resync)*time.Second + 10*time.Second
	waitUntil(t, "the restarted member to recover on its own", bound, func() bool { return n3.list() == nil })
	record(t, "recovered %s after start (resync %ds)", time.Since(t0).Round(100*time.Millisecond), fastTiming.Resync)
	if st := c.emu.Stats(); st.RefreshCalls != 0 {
		t.Fatalf("recovery needed a provider refresh: %+v", st)
	}
}

// BB8 (T2): every frame towards n3 is lost for good while the refresher
// rotates; the path then comes back. n3 must converge from the catalogue alone.
func TestBB8_LostPushIsRepairedByAntiEntropyAndResync(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s8-lost-push", ttl, fastTiming, newBin(t), true, 2)
	n2, n3 := c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)

	c.proxies["n3"].setMode(modeDrop)
	before := c.emu.Stats().RefreshOK
	waitUntil(t, "a rotation while n3 is cut off", 40*time.Second, func() bool { return c.emu.Stats().RefreshOK > before })
	time.Sleep(2 * time.Second)
	c.emu.ResetStats()
	c.proxies["n3"].setMode(modePass)
	c.proxies["n3"].killAll()
	waitUntil(t, "n3 to serve requests with the rotated pair", 60*time.Second, func() bool { return n3.list() == nil })
	if n3.refreshOK() != 0 {
		t.Fatalf("n3 spent a refresh_token")
	}
	waitUntil(t, "everyone converged", 30*time.Second, c.converged)
}
