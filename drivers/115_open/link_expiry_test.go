package _115_open

import (
	"fmt"
	"testing"
	"time"
)

// 115 CDN download URLs carry a Unix timestamp in `t=...` that marks when
// the signed URL stops accepting requests. The actual lifetime is typically
// just a few minutes — much shorter than OpenList's default link-cache TTL.
// `parseCDNExpiry` reads `t=` and returns the time-to-live to plug into
// model.Link.Expiration so OP refreshes the link before the CDN does.

func TestParseCDNExpiry_FutureTimestamp(t *testing.T) {
	future := time.Now().Add(30 * time.Minute).Unix()
	url := fmt.Sprintf("https://cdnfhnfdfs.115cdn.net/group518/foo.mkv?t=%d&u=123&s=52428800", future)

	d, ok := parseCDNExpiry(url)
	if !ok {
		t.Fatalf("ok = false, want true for URL with future t=")
	}
	// Expect 30min minus the safety margin. Allow ±5s slack for the
	// time.Now() jitter between Unix() above and time.Until() inside.
	want := 30*time.Minute - cdnExpirySafetyMargin
	if d < want-5*time.Second || d > want+5*time.Second {
		t.Fatalf("d = %v, want ~%v", d, want)
	}
}

func TestParseCDNExpiry_PastTimestamp_ClampedToMinimum(t *testing.T) {
	past := time.Now().Add(-10 * time.Minute).Unix()
	url := fmt.Sprintf("https://cdn.example.com/foo?t=%d", past)

	d, ok := parseCDNExpiry(url)
	if !ok {
		t.Fatalf("ok = false, want true even for expired t= (caller decides what to do)")
	}
	if d != cdnExpiryMinimum {
		t.Fatalf("d = %v, want clamp to cdnExpiryMinimum=%v", d, cdnExpiryMinimum)
	}
}

func TestParseCDNExpiry_AboutToExpire_ClampedToMinimum(t *testing.T) {
	// 30 seconds in the future, less than safety margin (60s) → would yield
	// a negative duration. Should clamp.
	soon := time.Now().Add(30 * time.Second).Unix()
	url := fmt.Sprintf("https://cdn.example.com/foo?t=%d", soon)

	d, ok := parseCDNExpiry(url)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if d != cdnExpiryMinimum {
		t.Fatalf("d = %v, want clamp to cdnExpiryMinimum=%v", d, cdnExpiryMinimum)
	}
}

func TestParseCDNExpiry_MissingT(t *testing.T) {
	d, ok := parseCDNExpiry("https://cdn.example.com/foo?u=123&s=52428800")
	if ok {
		t.Fatalf("ok = true, want false for URL without t=")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestParseCDNExpiry_MalformedT(t *testing.T) {
	d, ok := parseCDNExpiry("https://cdn.example.com/foo?t=notanumber&u=123")
	if ok {
		t.Fatalf("ok = true, want false for malformed t=")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestParseCDNExpiry_EmptyT(t *testing.T) {
	d, ok := parseCDNExpiry("https://cdn.example.com/foo?t=&u=123")
	if ok {
		t.Fatalf("ok = true, want false for empty t=")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestParseCDNExpiry_NegativeT(t *testing.T) {
	d, ok := parseCDNExpiry("https://cdn.example.com/foo?t=-1")
	if ok {
		// negative parses as Int64 but maps to a past time → ok=true with clamp
		if d != cdnExpiryMinimum {
			t.Fatalf("d = %v, want clamp to cdnExpiryMinimum=%v", d, cdnExpiryMinimum)
		}
	} else {
		// alternative acceptable behavior: reject negative as malformed.
		// Either policy is defensible; pin whichever the implementation chose.
		if d != 0 {
			t.Fatalf("d = %v, want 0", d)
		}
	}
}

func TestParseCDNExpiry_InvalidURL(t *testing.T) {
	// url.Parse is permissive — most "garbage in" still parses, but the
	// query bag is empty so t= is missing.
	d, ok := parseCDNExpiry("not_a_url_at_all")
	if ok {
		t.Fatalf("ok = true, want false for garbage URL with no query")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestParseCDNExpiry_RealWorld115URL(t *testing.T) {
	// Anchored on the actual URL the user pasted, but with t= rewritten to a
	// known future point so the test isn't time-bombed.
	future := time.Now().Add(2 * time.Hour).Unix()
	url := fmt.Sprintf(
		"https://cdnfhnfdfs.115cdn.net/group518/M00/5B/9F/tzyQp1JGFCUAAAAIj4qFUklVFs09176478/Django%%20Unchained%%202012.mkv?t=%d&u=103088508&s=52428800&d=vip-2559104837-cqghjg71avvncddhi-1-100195313&c=2&f=1&k=44423951d519b201c3b42e03556e724b&us=62914560&uc=10&v=1",
		future,
	)
	d, ok := parseCDNExpiry(url)
	if !ok {
		t.Fatalf("ok = false on real-world URL")
	}
	if d <= 0 || d > 2*time.Hour {
		t.Fatalf("d = %v, want in (0, 2h]", d)
	}
}

func TestParseCDNExpiry_SafetyMarginApplied(t *testing.T) {
	// Verify the margin actually shrinks the returned duration. Use a fixed
	// offset much larger than the safety margin so jitter doesn't matter.
	raw := 1 * time.Hour
	future := time.Now().Add(raw).Unix()
	url := fmt.Sprintf("https://cdn.example.com/foo?t=%d", future)
	d, ok := parseCDNExpiry(url)
	if !ok {
		t.Fatalf("ok = false")
	}
	if d >= raw {
		t.Fatalf("d = %v, expected strictly less than raw=%v (safety margin not applied)", d, raw)
	}
	if d < raw-2*cdnExpirySafetyMargin {
		t.Fatalf("d = %v, expected ≥ raw-2*margin=%v (margin too aggressive)", d, raw-2*cdnExpirySafetyMargin)
	}
}
