// Package session manages the login sessions ("devices") of users.
//
// A session is a row created when a JWT is issued; its ID is the token's jti.
// Requests are checked against an in-memory view of the table that is
// refreshed every few seconds, and a session's activity is written at most once
// per Interval, so an authenticated request costs no database round trip in the
// common case.
//
// References: AList c64f899a / 8623da53 / 9a7c82a7 (limit and eviction policy,
// settings), Nextcloud PublicKeyTokenProvider::updateTokenActivity and its 10 s
// token cache (throttled activity write, short-lived cache), Jellyfin
// AuthorizationContext (device row per login, throttled DateLastActivity).
package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	// DefaultTTL is how long a session may be idle before it stops counting and
	// stops working (device_session_ttl, seconds).
	DefaultTTL = 86400
	// cacheTTL bounds how stale a revocation made by another process can be.
	cacheTTL = 10 * time.Second
	// cleanupEvery is the period of the idle-session sweep.
	cleanupEvery = time.Hour
)

// Overridden by tests.
var (
	// Interval is the minimum time between two activity writes of one session.
	Interval = 60 * time.Second
	now      = time.Now
)

// Meta describes the client of a login or a request.
type Meta struct {
	ClientID  string // device id reported by the client (Client-Id), may be empty
	UserAgent string
	IP        string
}

type entry struct {
	userID      uint
	exists      bool
	active      bool
	lastActive  int64
	lastWritten time.Time
	loaded      time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]*entry{}
	// createMu serialises the limit check and the insert of a login.
	createMu sync.Mutex
)

func ttl() int64 {
	v := setting.GetInt(conf.DeviceSessionTTL, DefaultTTL)
	if v < 0 {
		return 0
	}
	return int64(v)
}

// since is the oldest last_active that still counts as in use (0 = no limit).
func since() int64 {
	t := ttl()
	if t == 0 {
		return 0
	}
	return now().Unix() - t
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Create starts a session for a login and returns its ID.
//
// A client that reports the same Client-Id as an existing session of the user
// replaces it (the old token stops working, the device count is unchanged).
// Otherwise max_devices applies: "deny" refuses the login with
// errs.TooManyDevices, "evict_oldest" revokes the least recently used session.
func Create(userID uint, m Meta) (string, error) {
	createMu.Lock()
	defer createMu.Unlock()

	if m.ClientID != "" {
		ids, err := db.MarkClientSessionsInactive(userID, m.ClientID)
		if err != nil {
			return "", err
		}
		for _, id := range ids {
			setInactive(id, userID)
		}
	}
	if limit := setting.GetInt(conf.MaxDevices, 0); limit > 0 {
		count, err := db.CountActiveSessionsByUser(userID, since())
		if err != nil {
			return "", err
		}
		for ; count >= int64(limit); count-- {
			if setting.GetStr(conf.DeviceEvictPolicy, "deny") != "evict_oldest" {
				return "", errors.WithStack(errs.TooManyDevices)
			}
			oldest, err := db.GetOldestActiveSession(userID, since())
			if err != nil {
				return "", err
			}
			if _, err := db.MarkSessionInactive(userID, oldest.SessionID); err != nil {
				return "", err
			}
			setInactive(oldest.SessionID, userID)
		}
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	t := now().Unix()
	s := &model.Session{
		SessionID: id, UserID: userID, ClientID: m.ClientID, UserAgent: clip(m.UserAgent, 255),
		IP: utils.MaskIP(m.IP), CreatedAt: t, LastActive: t, Status: model.SessionActive,
	}
	if err := db.CreateSession(s); err != nil {
		return "", err
	}
	mu.Lock()
	cache[id] = &entry{userID: userID, exists: true, active: true, lastActive: t, lastWritten: now(), loaded: now()}
	mu.Unlock()
	return id, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// setInactive makes a revocation visible to this process at once.
func setInactive(id string, userID uint) {
	mu.Lock()
	defer mu.Unlock()
	if e, ok := cache[id]; ok {
		e.active = false
		e.loaded = now()
		return
	}
	cache[id] = &entry{userID: userID, exists: true, active: false, loaded: now()}
}

// Check validates the session of a request authenticated by a JWT and records
// its activity. It returns errs.SessionInactive when the session is unknown,
// revoked, belongs to someone else, or has been idle longer than the TTL.
func Check(id string, userID uint, m Meta) error {
	if id == "" {
		return errors.WithStack(errs.SessionInactive)
	}
	t := now()
	mu.Lock()
	e := cache[id]
	if e == nil || t.Sub(e.loaded) > cacheTTL {
		mu.Unlock()
		fresh, err := load(id, t)
		if err != nil {
			return err
		}
		mu.Lock()
		// A write of this process may be newer than the row we just read.
		if old := cache[id]; old != nil && old.lastWritten.After(fresh.lastWritten) {
			fresh.lastWritten = old.lastWritten
			fresh.lastActive = max(fresh.lastActive, old.lastActive)
		}
		cache[id] = fresh
		e = fresh
	}
	ok := e.exists && e.active && e.userID == userID
	if ok {
		if w := ttl(); w > 0 && t.Unix()-e.lastActive > w {
			ok = false
		}
	}
	write := false
	if ok && t.Sub(e.lastWritten) >= Interval {
		e.lastWritten = t
		e.lastActive = t.Unix()
		write = true
	}
	mu.Unlock()
	if !ok {
		return errors.WithStack(errs.SessionInactive)
	}
	if write {
		if err := db.UpdateSessionActivity(id, t.Unix(), clip(m.UserAgent, 255), utils.MaskIP(m.IP)); err != nil {
			log.Warnf("update session activity: %v", err)
		}
	}
	return nil
}

func load(id string, t time.Time) (*entry, error) {
	s, err := db.GetSession(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &entry{loaded: t}, nil
		}
		return nil, err
	}
	return &entry{
		userID: s.UserID, exists: true, active: s.Status == model.SessionActive,
		lastActive: s.LastActive, lastWritten: time.Unix(s.LastActive, 0), loaded: t,
	}, nil
}

// Revoke ends a session. userID 0 means any owner (administrators); otherwise
// the session must belong to userID. It reports whether a session was revoked.
func Revoke(userID uint, id string) (bool, error) {
	n, err := db.MarkSessionInactive(userID, id)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	owner := userID
	if owner == 0 {
		if s, err := db.GetSession(id); err == nil {
			owner = s.UserID
		}
	}
	setInactive(id, owner)
	return true, nil
}

// List returns the in-use sessions of userID (everyone's when userID is 0).
func List(userID uint) ([]model.Session, error) {
	return db.ListSessions(userID, since())
}

// Cleanup deletes sessions idle for longer than the TTL and forgets cache
// entries that are not needed any more. It returns the number of rows removed.
func Cleanup() (int64, error) {
	t := now()
	mu.Lock()
	for id, e := range cache {
		if t.Sub(e.loaded) > 10*cacheTTL {
			delete(cache, id)
		}
	}
	mu.Unlock()
	w := ttl()
	if w == 0 {
		return 0, nil
	}
	return db.DeleteSessionsIdleBefore(t.Unix() - w)
}

// Reset drops the in-memory view (tests).
func Reset() {
	mu.Lock()
	cache = map[string]*entry{}
	mu.Unlock()
}

var stop chan struct{}

// StartCleanup runs Cleanup every hour until Stop.
func StartCleanup() {
	mu.Lock()
	if stop != nil {
		mu.Unlock()
		return
	}
	stop = make(chan struct{})
	ch := stop
	mu.Unlock()
	go func() {
		tk := time.NewTicker(cleanupEvery)
		defer tk.Stop()
		for {
			select {
			case <-ch:
				return
			case <-tk.C:
				if n, err := Cleanup(); err != nil {
					log.Warnf("session cleanup: %v", err)
				} else if n > 0 {
					log.Debugf("session cleanup: removed %d idle sessions", n)
				}
			}
		}
	}()
}

// Stop ends the cleanup loop.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if stop != nil {
		close(stop)
		stop = nil
	}
}
