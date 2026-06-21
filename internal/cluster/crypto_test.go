package cluster

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := deriveAEADKey([]byte("super-secret-cluster-key"))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"token":"abc123","secret":"xyz"}`)
	aad := []byte("aad-context")
	blob, err := seal(key, plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(key, blob, aad)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: %q != %q", got, plain)
	}
}

func TestOpenFailsWithWrongKey(t *testing.T) {
	k1, _ := deriveAEADKey([]byte("key-one"))
	k2, _ := deriveAEADKey([]byte("key-two"))
	blob, _ := seal(k1, []byte("hello"), nil)
	if _, err := open(k2, blob, nil); err == nil {
		t.Fatal("expected open to fail with a different cluster key")
	}
}

func TestOpenFailsWithTamperedAAD(t *testing.T) {
	key, _ := deriveAEADKey([]byte("k"))
	blob, _ := seal(key, []byte("hello"), []byte("aad-1"))
	if _, err := open(key, blob, []byte("aad-2")); err == nil {
		t.Fatal("expected open to fail when aad differs")
	}
}

func TestDeriveAEADKeyDeterministicAndRejectsEmpty(t *testing.T) {
	a, err := deriveAEADKey([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := deriveAEADKey([]byte("same"))
	if !bytes.Equal(a, b) {
		t.Fatal("key derivation must be deterministic across nodes")
	}
	if len(a) != aeadKeyN {
		t.Fatalf("derived key length = %d, want %d", len(a), aeadKeyN)
	}
	if _, err := deriveAEADKey(nil); err == nil {
		t.Fatal("empty PSK must be rejected")
	}
}

func TestIdentityNodeIDBoundToPubKey(t *testing.T) {
	id, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// node id is the hash of the pubkey — re-deriving must match.
	if nodeIDFromPub(id.Pub) != id.NodeID {
		t.Fatal("node id is not bound to pubkey")
	}
	// persistence round-trip via seed.
	id2, err := identityFromSeed(id.seed())
	if err != nil {
		t.Fatal(err)
	}
	if id2.NodeID != id.NodeID {
		t.Fatal("identity not stable across seed reload")
	}
}

func TestSignVerify(t *testing.T) {
	id, _ := newIdentity()
	msg := []byte("authentic message")
	sig := id.sign(msg)
	if !verifySig(id.Pub, msg, sig) {
		t.Fatal("valid signature rejected")
	}
	if verifySig(id.Pub, []byte("tampered"), sig) {
		t.Fatal("signature verified against the wrong message")
	}
	other, _ := newIdentity()
	if verifySig(other.Pub, msg, sig) {
		t.Fatal("signature verified under the wrong pubkey")
	}
}
