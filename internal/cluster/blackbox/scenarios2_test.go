//go:build blackbox

package blackbox

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// prime gives the refresher a first refresh (expires_in becomes known and a
// versioned record exists) and waits for every member to hold it.
func prime(t *testing.T, c *cluster, holder *node) {
	t.Helper()
	c.emu.ExpireAccess()
	if err := holder.list(); err != nil {
		t.Fatalf("prime refresh: %v", err)
	}
	waitUntil(t, "first pair held by every member", 30*time.Second, c.converged)
}

func (n *node) contains(sub string) bool { return strings.Contains(n.log(), sub) }

func TestBB2_HolderDownFollowerTakesOverPullsFirstRefreshesOnce(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s2-holderdown", ttl, fastTiming, newBin(t), true, 2)
	n1, n2, n3 := c.nodes[0], c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)
	pollers := []*poller{startPoller(n1), startPoller(n3)}

	// Who should take over: the survivors ranked by node id (none preferred, both working).
	expect, other := n1, n3
	if n3.nodeID < n1.nodeID {
		expect, other = n3, n1
	}
	before := c.emu.Stats().RefreshOK
	killedAt := time.Now()
	n2.stop() // killed by PID: no graceful lease release
	record(t, "killed refresher n2 (pid %d); expected successor by node-id order: %s", n2.pid(), expect.name)

	waitUntil(t, expect.name+" to take over", 60*time.Second, func() bool {
		for _, n := range []*node{n1, n3} {
			if c.holderSeenBy(n) != expect.nodeID {
				return false
			}
		}
		return true
	})
	takeover := time.Since(killedAt)
	record(t, "takeover visible on both survivors %s after the kill (lease TTL %ds)", takeover.Round(100*time.Millisecond), fastTiming.LeaseTTL)
	if takeover < time.Duration(fastTiming.LeaseTTL)*time.Second {
		t.Fatalf("took over after %s, before the dead holder's lease could have expired", takeover)
	}

	// Exactly one refresh by the successor; the other survivor never spends a token.
	waitUntil(t, "one refresh by the successor", 40*time.Second, func() bool { return c.emu.Stats().RefreshOK > before })
	time.Sleep(8 * time.Second) // well inside the next 16 s proactive interval
	st := c.emu.Stats()
	record(t, "passport refreshes after the kill: %d ok, fails=%v; successor %s logged %d, other survivor %s logged %d",
		st.RefreshOK-before, st.RefreshFail, expect.name, expect.refreshOK(), other.name, other.refreshOK())
	for _, p := range pollers {
		p.Stop()
		record(t, "survivor %s list requests: ok=%d fail=%d", p.n.name, p.ok.Load(), p.fail.Load())
	}
	if st.RefreshOK-before != 1 {
		t.Fatalf("want exactly 1 refresh after takeover, got %d", st.RefreshOK-before)
	}
	if other.refreshOK() != 0 {
		t.Fatalf("the non-holder survivor refreshed")
	}
	if st.RefreshFail[40140120] != 0 {
		t.Fatalf("a dead refresh token was presented: %+v", st.RefreshFail)
	}
	// Pull before refresh: the takeover log line precedes the lease, and a pull was sent.
	log := expect.log()
	i, j := strings.Index(log, "lease takeover begin"), strings.Index(log, "lease acquired")
	if i < 0 || j < i {
		t.Fatalf("takeover must log 'begin ... pulling latest credential first' before 'lease acquired' (i=%d j=%d)", i, j)
	}
	if !strings.Contains(log[i:j], "pull group=g115") {
		t.Fatalf("no pull between takeover begin and lease acquired")
	}
	waitUntil(t, "survivors converged", 30*time.Second, c.converged)
}

func TestBB3_FollowerRejectedRecoversViaRefreshRequest(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s3-follower401", ttl, fastTiming, newBin(t), true, 2)
	n1, n2, n3 := c.nodes[0], c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)

	c.emu.ResetStats()
	c.emu.ExpireAccess()
	t0 := time.Now()
	if err := n3.list(); err == nil {
		t.Fatal("the follower's request must be rejected: it may not refresh")
	} else {
		record(t, "follower n3 request after the token lapsed: %v", err)
	}
	// Recovery: pull finds nothing newer; after the wait the follower asks the refresher.
	waitUntil(t, "follower n3 to recover", 40*time.Second, func() bool { return n3.list() == nil })
	total := time.Since(t0)
	st := c.emu.Stats()
	record(t, "n3 recovered after %s (refresh-request wait %ds); passport calls=%d ok=%d fails=%v", total.Round(100*time.Millisecond), fastTiming.ReqWait, st.RefreshCalls, st.RefreshOK, st.RefreshFail)
	if st.RefreshCalls != 1 || st.RefreshOK != 1 {
		t.Fatalf("want exactly one refresh (by the refresher), got %+v", st)
	}
	if n3.refreshOK() != 0 || n1.refreshOK() != 0 || n2.refreshOK() != 1 {
		t.Fatalf("refresh attribution wrong: n1=%d n2=%d n3=%d", n1.refreshOK(), n2.refreshOK(), n3.refreshOK())
	}
	if !n2.contains("trigger=refresh-request") {
		t.Fatalf("the refresher's log must attribute the refresh to the refresh-request")
	}
	waitUntil(t, "everyone converged", 20*time.Second, c.converged)
	g3, _ := n3.group(c.groupID)
	g2, _ := n2.group(c.groupID)
	var sent, recv bool
	for _, r := range g3.Requests {
		if r.Direction == "sent" {
			sent = true
		}
	}
	for _, r := range g2.Requests {
		if r.Direction == "received" && strings.Contains(r.Result, "refresh started") {
			recv = true
		}
	}
	record(t, "request log: n3 sent=%v, n2 received+refreshed=%v", sent, recv)
	if !sent || !recv {
		t.Fatalf("refresh-request not visible in the status of both ends (sent=%v received=%v)", sent, recv)
	}
	// And the non-refresher never revoked the still-good refresh token.
	if n3.contains("[cluster] revoke group") || n1.contains("[cluster] revoke group") {
		t.Fatalf("an expired access token on a follower revoked the pair cluster-wide")
	}
}

func TestBB4a_PushLostWhileLinkStallsThenConverges(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s4a-stall", ttl, fastTiming, newBin(t), true, 2)
	n2, n3 := c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)
	p3 := startPoller(n3)
	defer p3.Stop()

	// n3's path to the hub silently stops delivering.
	c.proxies["n3"].setMode(modeStall)
	before := c.emu.Stats().RefreshOK
	waitUntil(t, "the refresher's proactive refresh while n3 is cut off", 40*time.Second, func() bool { return c.emu.Stats().RefreshOK > before })
	time.Sleep(3 * time.Second)
	g3, _ := n3.group(c.groupID)
	g2, _ := n2.group(c.groupID)
	record(t, "while stalled: n2 group version %d, n3 still sees %d; n3 list failures so far %d", g2.Version, g3.Version, p3.fail.Load())
	if g3.Version >= g2.Version {
		t.Fatalf("test premise: n3 must be behind while cut off")
	}
	c.proxies["n3"].setMode(modePass)
	restored := time.Now()
	waitUntil(t, "n3 to converge after the path is restored", 60*time.Second, c.converged)
	record(t, "n3 converged %s after the path was restored", time.Since(restored).Round(10*time.Millisecond))
	for _, n := range c.nodes {
		if n.contains("send queue full") {
			t.Fatalf("%s logged a 'send queue full' disconnect", n.name)
		}
	}
	waitUntil(t, "n3 requests to succeed again", 30*time.Second, func() bool { return n3.list() == nil })
	if c.emu.Stats().RefreshFail[40140120] != 0 {
		t.Fatalf("a stale refresh token was presented to passport: %+v", c.emu.Stats().RefreshFail)
	}
	if n3.refreshOK() != 0 {
		t.Fatalf("n3 refreshed while cut off")
	}
}

func TestBB4b_LinkKilledRepeatedlyStillConverges(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s4b-kill", ttl, fastTiming, newBin(t), true, 2)
	n2, n3 := c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // kill n3's link every 1.5 s, like the 10-18 flapping
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(1500 * time.Millisecond):
				c.proxies["n3"].killAll()
			}
		}
	}()
	before := c.emu.Stats().RefreshOK
	waitUntil(t, "proactive refresh during the kill storm", 40*time.Second, func() bool { return c.emu.Stats().RefreshOK > before })
	time.Sleep(4 * time.Second)
	close(stop)
	wg.Wait()
	storm := c.proxies["n3"].kills
	last := time.Now()
	waitUntil(t, "convergence once links stop being killed", 60*time.Second, c.converged)
	record(t, "n3's link was killed %d times; converged %s after the last kill", storm, time.Since(last).Round(10*time.Millisecond))
	st3, _ := n3.status()
	for _, l := range st3.Links {
		record(t, "n3 link to %s: connected=%v disconnects=%d last_reason=%q", l.NodeID[:6], l.Connected, l.Disconnects, l.LastReason)
	}
	for _, n := range c.nodes {
		if n.contains("send queue full") {
			t.Fatalf("%s logged 'send queue full'", n.name)
		}
	}
	if n3.refreshOK() != 0 {
		t.Fatal("n3 refreshed on its own while its link flapped")
	}
}

func TestBB4c_BurstOfControlTrafficDropsNoLink(t *testing.T) {
	c := buildCluster(t, "s4c-burst", 30*time.Second, fastTiming, newBin(t), true, 2)
	n1, n2, n3 := c.nodes[0], c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2)
	prime(t, c, n2)
	disc := func(n *node) (total int) {
		st, _ := n.status()
		for _, l := range st.Links {
			total += l.Disconnects
		}
		return
	}
	before := map[string]int{n1.name: disc(n1), n2.name: disc(n2), n3.name: disc(n3)}
	// 600 admin changes fired from all three nodes at once: preferred-refresher
	// policies and the resulting announce/inventory traffic. The old per-link FIFO
	// held 128 frames.
	var wg sync.WaitGroup
	t0 := time.Now()
	for _, n := range c.nodes {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				target := n2
				if i%2 == 1 {
					target = n3
				}
				_, _ = n.do("POST", "/api/admin/cluster/refresher", map[string]any{"group_id": c.groupID, "node_id": target.nodeID})
			}
		}()
	}
	wg.Wait()
	record(t, "600 control changes sent in %s", time.Since(t0).Round(time.Millisecond))
	c.setPreferred(n2)
	time.Sleep(8 * time.Second)
	for _, n := range c.nodes {
		if n.contains("send queue full") {
			t.Fatalf("%s: 'send queue full' disconnect during the burst", n.name)
		}
		if d := disc(n) - before[n.name]; d != 0 {
			t.Fatalf("%s: %d link disconnect(s) during the burst", n.name, d)
		}
	}
	record(t, "no link dropped on any node during the burst; no 'send queue full' in any log")
	waitUntil(t, "cluster converged and refresher agreed", 30*time.Second, func() bool {
		if !c.converged() {
			return false
		}
		h := c.holderSeenBy(n1)
		return h != "" && c.holderSeenBy(n2) == h && c.holderSeenBy(n3) == h
	})
}
