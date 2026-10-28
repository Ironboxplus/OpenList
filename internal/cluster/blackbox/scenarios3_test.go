//go:build blackbox

package blackbox

import (
	"os"
	"testing"
	"time"
)

// BB5: the hub still runs the old build (c02580c4: no leases, no directed
// messages, no refresh-requests; it relays credentials and group documents).
// Two new nodes sit behind it. The old hub only relays: it has no 115 storage.
//
// What must hold: credentials still flow through the old hub (a refresh by one
// new node reaches the other), the link status marks the old hub as legacy, and no refresh_token is presented twice (no 40140120).
func TestBB5_OldHubNewFollowers(t *testing.T) {
	if _, err := os.Stat(oldBin(t)); err != nil {
		t.Skipf("old binary not built: %v", err)
	}
	const ttl = 20 * time.Second
	c := buildCluster(t, "s5-oldhub", ttl, fastTiming, oldBin(t), false, 2) // the old build has no test base-URL hook, so the hub has no 115 storage
	n1, n2, n3 := c.nodes[0], c.nodes[1], c.nodes[2]

	waitUntil(t, "n2's link status to mark the old hub as legacy", 40*time.Second, func() bool {
		st, err := n2.status()
		if err != nil {
			return false
		}
		for _, l := range st.Links {
			if l.NodeID == n1.nodeID && l.Connected && l.Proto == 1 {
				return true
			}
		}
		return false
	})
	record(t, "n2's link to the old hub reports proto=1 (legacy)")

	// Only n2 is used. Its first expiry signal: it is the only node asking, so
	// whichever lease it holds, exactly one refresh must happen.
	c.emu.ResetStats()
	c.emu.ExpireAccess()
	var lastErr error
	ok := waitUntil(t, "n2 to recover from an expired access token", 90*time.Second, func() bool {
		lastErr = n2.list()
		return lastErr == nil
	})
	st := c.emu.Stats()
	record(t, "n2 recovered after %s; passport calls=%d ok=%d fails=%v", ok.Round(100*time.Millisecond), st.RefreshCalls, st.RefreshOK, st.RefreshFail)
	waitUntil(t, "n3 to hold the new pair via the old hub", 40*time.Second, func() bool {
		g, found := n3.group(c.groupID)
		g2, _ := n2.group(c.groupID)
		return found && g.Version == g2.Version
	})
	if err := n3.list(); err != nil {
		t.Fatalf("n3 could not use the pair relayed by the old hub: %v", err)
	}
	st = c.emu.Stats()
	record(t, "after n3 adopted it: passport calls=%d ok=%d fails=%v; refresh ok in logs n2=%d n3=%d",
		st.RefreshCalls, st.RefreshOK, st.RefreshFail, n2.refreshOK(), n3.refreshOK())
	if st.RefreshFail[40140120] != 0 {
		t.Fatalf("a stale refresh token was presented to passport: %+v", st.RefreshFail)
	}
	if n2.refreshOK()+n3.refreshOK() != st.RefreshOK {
		t.Fatalf("log attribution does not add up")
	}

	// Then let the proactive cycle run for 40 s with both new nodes in use: with
	// no lease visible across the old hub the two must not both spend tokens.
	p2, p3 := startPoller(n2), startPoller(n3)
	time.Sleep(40 * time.Second)
	p2.Stop()
	p3.Stop()
	st = c.emu.Stats()
	record(t, "40 s of use by both new nodes behind the old hub: passport ok=%d fails=%v; n2 ok=%d fail=%d, n3 ok=%d fail=%d; refreshes by n2=%d n3=%d",
		st.RefreshOK, st.RefreshFail, p2.ok.Load(), p2.fail.Load(), p3.ok.Load(), p3.fail.Load(), n2.refreshOK(), n3.refreshOK())
}
