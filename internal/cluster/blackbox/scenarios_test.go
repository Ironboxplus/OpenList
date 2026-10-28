//go:build blackbox

package blackbox

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// poller issues a real directory listing on a node twice a second and counts
// failures, so scenarios can state exactly how many user requests failed.
type poller struct {
	n     *node
	ok    atomic.Int64
	fail  atomic.Int64
	last  atomic.Value
	stop  chan struct{}
	wg    sync.WaitGroup
	times []time.Time
}

func startPoller(n *node) *poller {
	p := &poller{n: n, stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			if err := n.list(); err != nil {
				p.fail.Add(1)
				p.last.Store(err.Error())
			} else {
				p.ok.Add(1)
			}
		}
	}()
	return p
}

func (p *poller) Stop() { close(p.stop); p.wg.Wait() }

// settleCluster waits for the preferred holder to be elected and visible on all nodes.
func settleCluster(t *testing.T, c *cluster, holder *node) {
	t.Helper()
	c.setPreferred(holder)
	d := c.waitHolder(holder, 60*time.Second)
	record(t, "refresher %s elected and visible on all nodes after %s", holder.name, d.Round(100*time.Millisecond))
	time.Sleep(3 * time.Second) // a freshly claimed lease settles for 2 s before it refreshes
}

func TestBB1_ProactiveRefreshFollowersAdoptNoFollowerCallsPassport(t *testing.T) {
	const ttl = 20 * time.Second
	c := buildCluster(t, "s1-proactive", ttl, fastTiming, newBin(t), true, 2)
	n1, n2, n3 := c.nodes[0], c.nodes[1], c.nodes[2]
	settleCluster(t, c, n2) // n2 is the refresher; n1 hub and n3 are followers

	// The pair the storages were created with has no known lifetime, so the
	// refresher must not refresh blindly. Prove it: 15 s pass, zero passport calls.
	time.Sleep(15 * time.Second)
	if st := c.emu.Stats(); st.RefreshCalls != 0 {
		t.Fatalf("refresher refreshed with unknown expiry: %+v", st)
	}
	record(t, "idle with unknown expiry: passport refresh calls = 0 (no blind refresh)")

	// First expiry signal: the access token lapses; the refresher itself sees it.
	pollers := []*poller{startPoller(n1), startPoller(n3)}
	c.emu.ResetStats()
	c.emu.ExpireAccess()
	if err := n2.list(); err != nil {
		t.Fatalf("refresher should recover transparently: %v", err)
	}
	waitUntil(t, "followers to adopt the first refreshed pair", 20*time.Second, c.converged)

	// Now let two full proactive cycles run (16 s each at 80% of 20 s).
	for _, p := range pollers {
		p.ok.Store(0)
		p.fail.Store(0)
	}
	var convergence []time.Duration
	g0, _ := n2.group(c.groupID)
	lastVer, lastCalls := g0.Version, c.emu.Stats().RefreshOK
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && len(convergence) < 2 {
		time.Sleep(30 * time.Millisecond)
		g, ok := n2.group(c.groupID)
		if !ok || g.Version <= lastVer {
			continue
		}
		st := c.emu.Stats()
		if st.RefreshOK <= lastCalls {
			continue
		}
		lastVer, lastCalls = g.Version, st.RefreshOK
		// Convergence: from the provider issuing the new pair until every member
		// reports holding the new version.
		issued := st.RefreshCallTimes[len(st.RefreshCallTimes)-1]
		for !c.converged() {
			if time.Since(issued) > 20*time.Second {
				t.Fatalf("followers did not adopt the proactive refresh within 20s")
			}
			time.Sleep(20 * time.Millisecond)
		}
		convergence = append(convergence, time.Since(issued).Round(10*time.Millisecond))
	}
	for _, p := range pollers {
		p.Stop()
	}
	if len(convergence) < 2 {
		t.Fatalf("only %d proactive refreshes in 40s", len(convergence))
	}
	st := c.emu.Stats()
	record(t, "passport calls: total=%d ok=%d (first signal + %d proactive); fails=%v", st.RefreshCalls, st.RefreshOK, len(convergence), st.RefreshFail)
	record(t, "per-node 'refresh ok' log lines: n1(hub, follower)=%d n2(refresher)=%d n3(follower)=%d", n1.refreshOK(), n2.refreshOK(), n3.refreshOK())
	record(t, "follower convergence after each proactive refresh: %v", convergence)
	record(t, "follower list requests while cycling: n1 ok=%d fail=%d, n3 ok=%d fail=%d (last error %v)",
		pollers[0].ok.Load(), pollers[0].fail.Load(), pollers[1].ok.Load(), pollers[1].fail.Load(), pollers[1].last.Load())
	if n1.refreshOK() != 0 || n3.refreshOK() != 0 {
		t.Fatalf("a follower spent a refresh_token (n1=%d n3=%d)", n1.refreshOK(), n3.refreshOK())
	}
	if n2.refreshOK() != st.RefreshOK {
		t.Fatalf("emulator saw %d refreshes but the refresher logged %d", st.RefreshOK, n2.refreshOK())
	}
	if st.RefreshFail[40140120] != 0 || st.RefreshFail[40140117] != 0 {
		t.Fatalf("refresh errors: %+v", st.RefreshFail)
	}
	// A proactive refresh is attributed correctly in the holder's log and status.
	if n2.count(`refresh ok mount=/115 trigger=proactive`) < 2 {
		t.Fatalf("expected >=2 proactive refreshes in the refresher's log")
	}
	if g, _ := n1.group(c.groupID); g.LastRefresh == nil || g.LastRefresh.NodeID != n2.nodeID || g.LastRefresh.Outcome != "ok" {
		t.Fatalf("hub's status must show the refresher's last refresh: %+v", g.LastRefresh)
	}
	_ = strings.TrimSpace
}
