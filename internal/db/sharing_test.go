package db

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func sharingTestDB(t *testing.T) {
	t.Helper()
	db = openTestDB(t, "x_") // as in production
	if err := db.AutoMigrate(&model.SharingDB{}); err != nil {
		t.Fatal(err)
	}
}

func rowOf(t *testing.T, id string) model.SharingDB {
	t.Helper()
	s, err := GetSharingById(id)
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func TestMigrateSharingPasswordsKeepsLinksWorking(t *testing.T) {
	sharingTestDB(t)
	// rows as an older binary wrote them: plaintext only
	for _, s := range []*model.SharingDB{
		{ID: "plain", Pwd: "hunter2"},
		{ID: "open"},
		{ID: "hashed"},
	} {
		if s.ID == "hashed" {
			s.SetPassword("already")
		}
		if _, err := CreateSharing(s); err != nil {
			t.Fatal(err)
		}
	}
	before := rowOf(t, "hashed")

	if err := migrateSharingPasswords(); err != nil {
		t.Fatal(err)
	}
	p := rowOf(t, "plain")
	if p.PasswordHash == "" || p.PasswordSalt == "" {
		t.Fatalf("no hash after migration: %+v", p)
	}
	if p.Pwd != "hunter2" {
		t.Fatalf("phase 1 keeps the plaintext column, got %q", p.Pwd)
	}
	if !p.VerifyPassword("hunter2") || p.VerifyPassword("hunter3") {
		t.Fatal("migrated hash does not verify the old password")
	}
	if o := rowOf(t, "open"); o.PasswordHash != "" || o.HasPassword() {
		t.Fatalf("open share got a password: %+v", o)
	}
	if h := rowOf(t, "hashed"); h.PasswordHash != before.PasswordHash || h.PasswordSalt != before.PasswordSalt {
		t.Fatal("an existing hash was rewritten")
	}

	// idempotent: a second start changes nothing
	if err := migrateSharingPasswords(); err != nil {
		t.Fatal(err)
	}
	if again := rowOf(t, "plain"); again.PasswordHash != p.PasswordHash || again.PasswordSalt != p.PasswordSalt {
		t.Fatal("second run rewrote the hash")
	}
}

func TestTakeSharingSlot(t *testing.T) {
	sharingTestDB(t)
	for _, s := range []*model.SharingDB{
		{ID: "two", MaxAccessed: 2},
		{ID: "burn", BurnAfterRead: true},
		{ID: "free"},
	} {
		if _, err := CreateSharing(s); err != nil {
			t.Fatal(err)
		}
	}
	take := func(id string) bool {
		ok, err := TakeSharingSlot(id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take("two") || rowOf(t, "two").ConsumedAt != nil {
		t.Fatal("first of two slots")
	}
	if !take("two") || rowOf(t, "two").ConsumedAt == nil {
		t.Fatal("second of two slots must stamp consumed_at")
	}
	if take("two") || rowOf(t, "two").Accessed != 2 {
		t.Fatal("third slot of two")
	}
	if !take("burn") || take("burn") {
		t.Fatal("burn after read is one slot")
	}
	if b := rowOf(t, "burn"); b.Accessed != 1 || b.ConsumedAt == nil || b.LastAccessAt == nil {
		t.Fatalf("burn row: %+v", b)
	}
	for range 5 {
		if !take("free") {
			t.Fatal("unlimited share refused")
		}
	}
	if f := rowOf(t, "free"); f.ConsumedAt != nil || f.Accessed != 5 {
		t.Fatalf("unlimited share: %+v", f)
	}
}

func TestAddSharingStats(t *testing.T) {
	sharingTestDB(t)
	if _, err := CreateSharing(&model.SharingDB{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := AddSharingStats("s", 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := AddSharingStats("s", 0, 2); err != nil {
		t.Fatal(err)
	}
	s := rowOf(t, "s")
	if s.ViewCount != 1 || s.DownloadCount != 2 || s.LastAccessAt == nil {
		t.Fatalf("%+v", s)
	}
}
