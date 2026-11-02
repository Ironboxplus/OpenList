package op_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

func newShare(t *testing.T, mod func(*model.SharingDB)) *model.Sharing {
	t.Helper()
	s := &model.Sharing{
		SharingDB: &model.SharingDB{},
		Files:     []string{"/a"},
		Creator:   &model.User{Permission: 1 << 14},
	}
	if mod != nil {
		mod(s.SharingDB)
	}
	if _, err := db.CreateSharing(s.SharingDB); err != nil {
		t.Fatal(err)
	}
	return s
}

func stored(t *testing.T, id string) *model.SharingDB {
	t.Helper()
	s, err := db.GetSharingById(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dl(ip string) op.SharingAccess { return op.SharingAccess{IP: ip, RangeStart: -1} }

func TestSharingPasswordHashVerifies(t *testing.T) {
	s := &model.SharingDB{}
	s.SetPassword("s3cret")
	if s.PasswordHash == "" || s.PasswordSalt == "" || s.Pwd != "s3cret" {
		t.Fatalf("SetPassword left %+v", s)
	}
	if !s.VerifyPassword("s3cret") || s.VerifyPassword("s3cre") || s.VerifyPassword("") {
		t.Fatal("hash verification wrong")
	}
	// the hash decides: a stale plaintext column does not open the share
	s.Pwd = "other"
	if s.VerifyPassword("other") || !s.VerifyPassword("s3cret") {
		t.Fatal("plaintext column was consulted although a hash exists")
	}
	// a row from an older binary has only the plaintext
	legacy := &model.SharingDB{Pwd: "old"}
	if !legacy.VerifyPassword("old") || legacy.VerifyPassword("new") {
		t.Fatal("legacy plaintext row not verified")
	}
	// no password: everything passes; clearing removes hash and salt
	open := &model.SharingDB{}
	if !open.VerifyPassword("") || open.HasPassword() {
		t.Fatal("open share wrong")
	}
	s.SetPassword("")
	if s.HasPassword() || s.PasswordHash != "" || s.PasswordSalt != "" {
		t.Fatalf("clearing left %+v", s)
	}
	// a new salt per password
	a, b := &model.SharingDB{}, &model.SharingDB{}
	a.SetPassword("same")
	b.SetPassword("same")
	if a.PasswordHash == b.PasswordHash {
		t.Fatal("two shares with the same password share a hash")
	}
}

func TestSharingTokenStandsForPassword(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	other := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	ip := "20.0.0.1"

	if _, err := op.AuthSharing(ip, "bad", s); !errors.Is(err, errs.WrongShareCode) {
		t.Fatalf("wrong password: %v", err)
	}
	tok, err := op.AuthSharing(ip, "pw", s)
	if err != nil || tok == "" {
		t.Fatalf("auth: %q %v", tok, err)
	}
	a := dl(ip)
	a.Token = tok
	if err := op.CheckSharingContent(a, s, true); err != nil {
		t.Fatalf("token rejected: %v", err)
	}
	if err := op.CheckSharingContent(a, other, true); !errors.Is(err, errs.WrongShareCode) {
		t.Fatalf("token of another share accepted: %v", err)
	}
	a.Token = tok + "x"
	if err := op.CheckSharingContent(a, s, true); !errors.Is(err, errs.WrongShareCode) {
		t.Fatalf("forged token accepted: %v", err)
	}

	// changing the password invalidates the tokens issued before
	a.Token = tok
	s.SetPassword("pw2")
	if err := op.CheckSharingContent(a, s, true); !errors.Is(err, errs.WrongShareCode) {
		t.Fatalf("token survived a password change: %v", err)
	}

	// an open share needs no token
	o := newShare(t, nil)
	if tok, err := op.AuthSharing(ip, "", o); err != nil || tok != "" {
		t.Fatalf("open share auth: %q %v", tok, err)
	}
}

func TestSharingTokenExpiresWithShare(t *testing.T) {
	soon := time.Now().Add(-time.Second)
	s := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	tok, err := op.AuthSharing("20.0.1.1", "pw", s)
	if err != nil {
		t.Fatal(err)
	}
	s.Expires = &soon
	a := dl("20.0.1.1")
	a.Token = tok
	if err := op.CheckSharingContent(a, s, true); !errors.Is(err, errs.InvalidSharing) {
		t.Fatalf("expired share accepted: %v", err)
	}
	// a token never outlives the share: signed for the time left
	later := time.Now().Add(time.Hour)
	s.Expires = &later
	tok = op.SignSharingToken(s.SharingDB)
	if err := op.VerifySharingToken(s.SharingDB, tok); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	s.Expires = &past
	if err := op.VerifySharingToken(s.SharingDB, op.SignSharingToken(s.SharingDB)); err == nil {
		t.Fatal("token for an expired share verified")
	}
}

func TestSharingWrongPasswordIsThrottledPerIPAndShare(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	other := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	ip := "30.0.0.1"

	for i := range model.DefaultMaxAuthRetries {
		a := dl(ip)
		a.Pwd = fmt.Sprintf("bad%d", i)
		if err := op.CheckSharingContent(a, s, false); !errors.Is(err, errs.WrongShareCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// locked out: even the right password is refused for now
	a := dl(ip)
	a.Pwd = "pw"
	if err := op.CheckSharingContent(a, s, false); !errors.Is(err, errs.TooManyShareAttempts) {
		t.Fatalf("locked out ip: %v", err)
	}
	// not other IPs, not other shares
	b := dl("30.0.0.2")
	b.Pwd = "pw"
	if err := op.CheckSharingContent(b, s, false); err != nil {
		t.Fatalf("other ip: %v", err)
	}
	if err := op.CheckSharingContent(a, other, false); err != nil {
		t.Fatalf("other share: %v", err)
	}
	// opening the page without a password (the prompt) is not an attempt
	for range 3 * model.DefaultMaxAuthRetries {
		if err := op.CheckSharingContent(dl("30.0.0.3"), s, false); !errors.Is(err, errs.WrongShareCode) {
			t.Fatalf("no password: %v", err)
		}
	}
	c := dl("30.0.0.3")
	c.Pwd = "pw"
	if err := op.CheckSharingContent(c, s, false); err != nil {
		t.Fatalf("prompt requests locked the ip: %v", err)
	}
}

func TestSharingRightPasswordResetsFailures(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw") })
	ip := "31.0.0.1"
	for round := range 3 {
		for range model.DefaultMaxAuthRetries - 1 {
			a := dl(ip)
			a.Pwd = "bad"
			_ = op.CheckSharingContent(a, s, false)
		}
		a := dl(ip)
		a.Pwd = "pw"
		if err := op.CheckSharingContent(a, s, false); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func TestSharingHeadNeverCounts(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.MaxAccessed = 1 })
	a := dl("40.0.0.1")
	a.Head = true
	for range 3 {
		if err := op.ChargeSharing(a, s); err != nil {
			t.Fatal(err)
		}
	}
	if d := stored(t, s.ID); d.Accessed != 0 || d.DownloadCount != 0 {
		t.Fatalf("HEAD counted: accessed %d downloads %d", d.Accessed, d.DownloadCount)
	}
	// and the real GET afterwards still has its slot
	if err := op.ChargeSharing(dl("40.0.0.1"), s); err != nil {
		t.Fatalf("GET after HEAD: %v", err)
	}
	if d := stored(t, s.ID); d.Accessed != 1 || d.DownloadCount != 1 {
		t.Fatalf("accessed %d downloads %d, want 1/1", d.Accessed, d.DownloadCount)
	}
}

func TestSharingRangedGetsAreOneSlotAndOneDownload(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.MaxAccessed = 1 })
	ip := "41.0.0.1"
	// a player: probes the tail, then reads from the middle, then from the start
	for _, start := range []int64{-2, 1000, 0, 5000, 0} {
		a := dl(ip)
		a.RangeStart = start
		if err := op.ChargeSharing(a, s); err != nil {
			t.Fatalf("range %d: %v", start, err)
		}
	}
	d := stored(t, s.ID)
	if d.Accessed != 1 {
		t.Fatalf("accessed = %d, want 1 slot for the whole session", d.Accessed)
	}
	// "downloads" = requests that start a file: the two bytes=0- requests
	if d.DownloadCount != 2 {
		t.Fatalf("download_count = %d, want 2", d.DownloadCount)
	}
	// a second visitor finds the limit reached
	if err := op.ChargeSharing(dl("41.0.0.2"), s); !errors.Is(err, errs.InvalidSharing) {
		t.Fatalf("second visitor: %v", err)
	}
}

func TestSharingRangeStartParse(t *testing.T) {
	for in, want := range map[string]int64{
		"":                 -1,
		"items=0-5":        -1,
		"bytes=0-":         0,
		"bytes=0-99":       0,
		" bytes=100-":      100,
		"bytes=5-9,20-30":  5,
		"bytes=-500":       -2,
		"bytes=x-1":        -2,
		"bytes=-":          -2,
		"bytes=99999999-9": 99999999,
	} {
		if got := op.RangeStart(in); got != want {
			t.Errorf("RangeStart(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSharingPreviewTakesSlotButIsNotADownload(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.MaxAccessed = 1 })
	a := dl("42.0.0.1")
	a.Preview = true
	if err := op.ChargeSharing(a, s); err != nil {
		t.Fatal(err)
	}
	d := stored(t, s.ID)
	if d.Accessed != 1 || d.DownloadCount != 0 {
		t.Fatalf("accessed %d downloads %d, want 1/0", d.Accessed, d.DownloadCount)
	}
	// the same visitor then downloading is the same slot, and now a download
	if err := op.ChargeSharing(dl("42.0.0.1"), s); err != nil {
		t.Fatal(err)
	}
	d = stored(t, s.ID)
	if d.Accessed != 1 || d.DownloadCount != 1 {
		t.Fatalf("accessed %d downloads %d, want 1/1", d.Accessed, d.DownloadCount)
	}
}

func TestSharingPreviewAndDownloadSwitches(t *testing.T) {
	prev := op.SharingAccess{IP: "43.0.0.1", Preview: true, RangeStart: -1}
	down := dl("43.0.0.1")

	noDownload := newShare(t, func(d *model.SharingDB) { d.DenyDownload = true })
	if err := op.ChargeSharing(down, noDownload); !errors.Is(err, errs.ShareActionNotAllowed) {
		t.Fatalf("download of a preview-only share: %v", err)
	}
	if err := op.CheckSharingContent(down, noDownload, true); !errors.Is(err, errs.ShareActionNotAllowed) {
		t.Fatalf("download gate of a preview-only share: %v", err)
	}
	if err := op.ChargeSharing(prev, noDownload); err != nil {
		t.Fatalf("preview of a preview-only share: %v", err)
	}

	noPreview := newShare(t, func(d *model.SharingDB) { d.DenyPreview = true })
	if err := op.ChargeSharing(prev, noPreview); !errors.Is(err, errs.ShareActionNotAllowed) {
		t.Fatalf("preview of a download-only share: %v", err)
	}
	if err := op.ChargeSharing(down, noPreview); err != nil {
		t.Fatalf("download of a download-only share: %v", err)
	}

	// refusing costs nothing; browsing (not content) is always allowed
	if d := stored(t, noDownload.ID); d.DownloadCount != 0 {
		t.Fatalf("refused download counted: %d", d.DownloadCount)
	}
	if err := op.CheckSharing("43.0.0.1", "", noDownload); err != nil {
		t.Fatalf("browsing a preview-only share: %v", err)
	}
	if !noDownload.AllowPreview() || noDownload.AllowDownload() || noPreview.AllowPreview() || !noPreview.AllowDownload() {
		t.Fatal("Allow* accessors")
	}
}

func TestSharingBurnAfterRead(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.BurnAfterRead = true })
	if s.EffectiveMaxAccessed() != 1 {
		t.Fatalf("effective max = %d", s.EffectiveMaxAccessed())
	}
	if err := op.ChargeSharing(dl("44.0.0.1"), s); err != nil {
		t.Fatal(err)
	}
	d := stored(t, s.ID)
	if d.Accessed != 1 || d.ConsumedAt == nil {
		t.Fatalf("accessed %d consumed_at %v", d.Accessed, d.ConsumedAt)
	}
	// the reader keeps reading (player ranges); everyone else is out
	s.SharingDB = d
	r := dl("44.0.0.1")
	r.RangeStart = 4096
	if err := op.ChargeSharing(r, s); err != nil {
		t.Fatalf("reader after burn: %v", err)
	}
	if err := op.CheckSharing("44.0.0.2", "", s); !errors.Is(err, errs.InvalidSharing) {
		t.Fatalf("second visitor browse: %v", err)
	}
	if err := op.ChargeSharing(dl("44.0.0.2"), s); !errors.Is(err, errs.InvalidSharing) {
		t.Fatalf("second visitor: %v", err)
	}
}

func TestSharingMaxAccessedWinsOverBurn(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.BurnAfterRead = true; d.MaxAccessed = 2 })
	for i := range 2 {
		if err := op.ChargeSharing(dl(fmt.Sprintf("45.0.0.%d", i)), s); err != nil {
			t.Fatalf("visitor %d: %v", i, err)
		}
	}
	if err := op.ChargeSharing(dl("45.0.0.9"), s); !errors.Is(err, errs.InvalidSharing) {
		t.Fatalf("third visitor: %v", err)
	}
}

func TestSharingFailedPasswordCostsNoSlot(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.SetPassword("pw"); d.MaxAccessed = 1 })
	a := dl("46.0.0.1")
	a.Pwd = "bad"
	if err := op.ChargeSharing(a, s); !errors.Is(err, errs.WrongShareCode) {
		t.Fatal(err)
	}
	if d := stored(t, s.ID); d.Accessed != 0 || d.DownloadCount != 0 {
		t.Fatalf("rejected request counted: %d/%d", d.Accessed, d.DownloadCount)
	}
}

func TestSharingBurnConcurrent(t *testing.T) {
	s := newShare(t, func(d *model.SharingDB) { d.BurnAfterRead = true })
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if op.ChargeSharing(dl(fmt.Sprintf("47.0.0.%d", i)), s) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if d := stored(t, s.ID); ok.Load() != 1 || d.Accessed != 1 || d.DownloadCount != 1 {
		t.Fatalf("granted %d accessed %d downloads %d, want 1/1/1", ok.Load(), d.Accessed, d.DownloadCount)
	}
}

func TestSharingViewCountOncePerVisitorWindow(t *testing.T) {
	s := newShare(t, nil)
	for range 5 {
		op.CountSharingView("48.0.0.1", s)
	}
	op.CountSharingView("48.0.0.2", s)
	d := stored(t, s.ID)
	if d.ViewCount != 2 || d.LastAccessAt == nil {
		t.Fatalf("view_count %d last_access %v, want 2", d.ViewCount, d.LastAccessAt)
	}
	// browsing is not a download
	if d.DownloadCount != 0 || d.Accessed != 0 {
		t.Fatalf("view counted a download: %+v", d)
	}
}

// A share saved from a copy loaded before a visit must not roll the counters back.
func TestSharingUpdateKeepsCounters(t *testing.T) {
	s := newShare(t, nil)
	old := stored(t, s.ID)
	op.CountSharingView("49.0.0.1", s)
	if err := op.ChargeSharing(dl("49.0.0.1"), s); err != nil {
		t.Fatal(err)
	}
	old.Remark = "edited"
	if err := db.UpdateSharing(old); err != nil {
		t.Fatal(err)
	}
	d := stored(t, s.ID)
	if d.Remark != "edited" || d.ViewCount != 1 || d.DownloadCount != 1 {
		t.Fatalf("remark %q views %d downloads %d", d.Remark, d.ViewCount, d.DownloadCount)
	}
}
