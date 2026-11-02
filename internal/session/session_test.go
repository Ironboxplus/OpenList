package session

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

var clock time.Time

// env gives each test a fresh database (with the production table prefix), an
// empty cache and a clock it controls.
func env(t *testing.T) {
	t.Helper()
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:session-%d?mode=memory&cache=shared", time.Now().UnixNano())),
		&gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "x_"}})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	Reset()
	clock = time.Unix(1_800_000_000, 0)
	oldNow, oldInterval := now, Interval
	now = func() time.Time { return clock }
	t.Cleanup(func() { now, Interval = oldNow, oldInterval; Reset() })
	set(t, conf.MaxDevices, "0")
	set(t, conf.DeviceEvictPolicy, "deny")
	set(t, conf.DeviceSessionTTL, "86400")
}

func set(t *testing.T, key, value string) {
	t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{Key: key, Value: value, Type: conf.TypeString, Group: model.GLOBAL}); err != nil {
		t.Fatal(err)
	}
}

func advance(d time.Duration) { clock = clock.Add(d) }

func mustCreate(t *testing.T, user uint, clientID string) string {
	t.Helper()
	id, err := Create(user, Meta{ClientID: clientID, UserAgent: "ua", IP: "10.1.2.3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

func check(id string, user uint) error {
	return Check(id, user, Meta{UserAgent: "ua2", IP: "10.9.9.9"})
}

func lastActive(t *testing.T, id string) int64 {
	t.Helper()
	s, err := db.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return s.LastActive
}

func TestActivityIsWrittenAtMostOncePerInterval(t *testing.T) {
	env(t)
	Interval = 60 * time.Second
	id := mustCreate(t, 1, "")
	start := clock.Unix()

	// 100 requests inside the interval, spread over 30 s (the cache expires
	// and reloads in between): nothing is written.
	for i := 0; i < 100; i++ {
		advance(300 * time.Millisecond)
		if err := check(id, 1); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := lastActive(t, id); got != start {
		t.Fatalf("last_active moved to %d inside the interval (start %d)", got-start, 0)
	}

	// The first request after the interval writes once, and the next 99 do not.
	advance(31 * time.Second)
	if err := check(id, 1); err != nil {
		t.Fatal(err)
	}
	written := lastActive(t, id)
	if written != clock.Unix() {
		t.Fatalf("last_active = %d, want %d", written, clock.Unix())
	}
	for i := 0; i < 99; i++ {
		advance(100 * time.Millisecond)
		if err := check(id, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := lastActive(t, id); got != written {
		t.Fatalf("further requests wrote again: %d -> %d", written, got)
	}
	// The device list shows the latest user agent and masked address.
	s, _ := db.GetSession(id)
	if s.UserAgent != "ua2" || s.IP != "10.*.*.9" {
		t.Fatalf("ua/ip = %q %q", s.UserAgent, s.IP)
	}
}

func TestCheckRejectsUnknownEmptyAndForeignSessions(t *testing.T) {
	env(t)
	id := mustCreate(t, 1, "")
	for name, err := range map[string]error{
		"empty id":   check("", 1),
		"unknown id": check("nope", 1),
		"other user": check(id, 2),
	} {
		if !errors.Is(err, errs.SessionInactive) {
			t.Errorf("%s: err = %v, want SessionInactive", name, err)
		}
	}
	if err := check(id, 1); err != nil {
		t.Fatalf("owner: %v", err)
	}
}

func TestDenyRefusesLoginOverTheLimit(t *testing.T) {
	env(t)
	set(t, conf.MaxDevices, "2")
	a := mustCreate(t, 1, "")
	b := mustCreate(t, 1, "")
	if _, err := Create(1, Meta{}); !errors.Is(err, errs.TooManyDevices) {
		t.Fatalf("third login: err = %v, want TooManyDevices", err)
	}
	if n, _ := db.CountActiveSessionsByUser(1, 0); n != 2 {
		t.Fatalf("a refused login left %d sessions, want 2", n)
	}
	if check(a, 1) != nil || check(b, 1) != nil {
		t.Fatal("existing sessions stopped working")
	}
	// the limit is per user
	if _, err := Create(2, Meta{}); err != nil {
		t.Fatalf("another user: %v", err)
	}
}

func TestEvictOldestSignsTheOldestOut(t *testing.T) {
	env(t)
	set(t, conf.MaxDevices, "2")
	set(t, conf.DeviceEvictPolicy, "evict_oldest")
	first := mustCreate(t, 1, "")
	advance(time.Minute)
	second := mustCreate(t, 1, "")
	advance(time.Minute)
	third := mustCreate(t, 1, "")

	if err := check(first, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("evicted session: err = %v, want SessionInactive (401)", err)
	}
	if check(second, 1) != nil || check(third, 1) != nil {
		t.Fatal("the newer sessions must keep working")
	}
	if n, _ := db.CountActiveSessionsByUser(1, 0); n != 2 {
		t.Fatalf("%d active sessions, want 2", n)
	}
}

func TestSameClientIDReplacesItsSession(t *testing.T) {
	env(t)
	set(t, conf.MaxDevices, "1")
	old := mustCreate(t, 1, "phone")
	// same device logs in again: allowed even at the limit, old token dies
	fresh := mustCreate(t, 1, "phone")
	if old == fresh {
		t.Fatal("a new login must get a new session id")
	}
	if err := check(old, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("replaced session: err = %v", err)
	}
	if err := check(fresh, 1); err != nil {
		t.Fatal(err)
	}
	// a different device is a different slot
	if _, err := Create(1, Meta{ClientID: "laptop"}); !errors.Is(err, errs.TooManyDevices) {
		t.Fatalf("second device: err = %v", err)
	}
	// the same client id of another user is unrelated
	if _, err := Create(2, Meta{ClientID: "phone"}); err != nil {
		t.Fatalf("other user, same client id: %v", err)
	}
}

func TestRevokeChecksTheOwner(t *testing.T) {
	env(t)
	id := mustCreate(t, 1, "")
	if ok, err := Revoke(2, id); err != nil || ok {
		t.Fatalf("another user revoked it: ok=%v err=%v", ok, err)
	}
	if check(id, 1) != nil {
		t.Fatal("session died though the wrong user asked")
	}
	if ok, err := Revoke(1, id); err != nil || !ok {
		t.Fatalf("owner revoke: ok=%v err=%v", ok, err)
	}
	if err := check(id, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("after revoke: %v", err)
	}
	other := mustCreate(t, 1, "")
	if ok, err := Revoke(0, other); err != nil || !ok { // 0 = administrator
		t.Fatalf("admin revoke: ok=%v err=%v", ok, err)
	}
	if err := check(other, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("after admin revoke: %v", err)
	}
	if ok, _ := Revoke(1, "does-not-exist"); ok {
		t.Fatal("revoked a session that does not exist")
	}
}

func TestIdleSessionsExpireStopCountingAndAreCleaned(t *testing.T) {
	env(t)
	set(t, conf.DeviceSessionTTL, "3600")
	set(t, conf.MaxDevices, "1")
	idle := mustCreate(t, 1, "")
	advance(2 * time.Hour)

	if err := check(idle, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("idle session: err = %v, want SessionInactive", err)
	}
	// an idle session does not occupy the single slot
	if _, err := Create(1, Meta{}); err != nil {
		t.Fatalf("login with only an idle session present: %v", err)
	}
	list, _ := List(1)
	if len(list) != 1 {
		t.Fatalf("List shows %d sessions, want only the live one", len(list))
	}
	n, err := Cleanup()
	if err != nil || n != 1 {
		t.Fatalf("Cleanup removed %d (err %v), want 1", n, err)
	}
	if _, err := db.GetSession(idle); err == nil {
		t.Fatal("idle row still in the table")
	}
}

func TestTTLZeroNeverExpires(t *testing.T) {
	env(t)
	set(t, conf.DeviceSessionTTL, "0")
	id := mustCreate(t, 1, "")
	advance(24 * 365 * time.Hour)
	if err := check(id, 1); err != nil {
		t.Fatalf("ttl 0: %v", err)
	}
	if n, err := Cleanup(); err != nil || n != 0 {
		t.Fatalf("Cleanup with ttl 0 removed %d (%v)", n, err)
	}
}

func TestSessionsSurviveARestart(t *testing.T) {
	env(t)
	id := mustCreate(t, 1, "")
	Reset() // what a restart does to the in-memory view
	if err := check(id, 1); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestRevocationByAnotherProcessIsSeenWithinTheCacheTTL(t *testing.T) {
	env(t)
	id := mustCreate(t, 1, "")
	if check(id, 1) != nil {
		t.Fatal("fresh session rejected")
	}
	// another process marks it inactive straight in the database
	if _, err := db.MarkSessionInactive(1, id); err != nil {
		t.Fatal(err)
	}
	if check(id, 1) != nil {
		t.Fatal("within the cache TTL the old answer is allowed")
	}
	advance(cacheTTL + time.Second)
	if err := check(id, 1); !errors.Is(err, errs.SessionInactive) {
		t.Fatalf("after the cache TTL: %v", err)
	}
}

func TestSessionTableUsesThePrefix(t *testing.T) {
	env(t)
	id := mustCreate(t, 1, "")
	var n int64
	if err := db.GetDb().Table("x_sessions").Where("session_id = ?", id).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("x_sessions rows = %d, err %v", n, err)
	}
}
