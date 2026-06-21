package cluster

import "testing"

func TestEnvelopeSealOpenRoundTrip(t *testing.T) {
	key, _ := deriveAEADKey([]byte("psk"))
	id, _ := newIdentity()
	rc := newReplayCache(replayWindowSec)
	msg := &syncMessage{Type: "push", Wants: []string{"/a", "/b"}}

	raw, err := sealEnvelope(key, id, msg, 1000)
	if err != nil {
		t.Fatal(err)
	}
	env, got, err := openEnvelope(key, raw, 1000, rc, replayWindowSec)
	if err != nil {
		t.Fatalf("open envelope failed: %v", err)
	}
	if env.Sender != id.NodeID {
		t.Fatal("sender id mismatch")
	}
	if got.Type != "push" || len(got.Wants) != 2 {
		t.Fatalf("payload not preserved: %+v", got)
	}
}

func TestEnvelopeReplayRejected(t *testing.T) {
	key, _ := deriveAEADKey([]byte("psk"))
	id, _ := newIdentity()
	rc := newReplayCache(replayWindowSec)
	raw, _ := sealEnvelope(key, id, &syncMessage{Type: "announce"}, 1000)

	if _, _, err := openEnvelope(key, raw, 1000, rc, replayWindowSec); err != nil {
		t.Fatalf("first open should succeed: %v", err)
	}
	if _, _, err := openEnvelope(key, raw, 1001, rc, replayWindowSec); err == nil {
		t.Fatal("replayed nonce must be rejected")
	}
}

func TestEnvelopeStaleTimestampRejected(t *testing.T) {
	key, _ := deriveAEADKey([]byte("psk"))
	id, _ := newIdentity()
	rc := newReplayCache(replayWindowSec)
	raw, _ := sealEnvelope(key, id, &syncMessage{Type: "announce"}, 1000)

	// now is far beyond the replay window from the envelope timestamp.
	if _, _, err := openEnvelope(key, raw, 1000+replayWindowSec+10, rc, replayWindowSec); err == nil {
		t.Fatal("stale timestamp must be rejected")
	}
}

func TestEnvelopeWrongKeyRejected(t *testing.T) {
	k1, _ := deriveAEADKey([]byte("psk-1"))
	k2, _ := deriveAEADKey([]byte("psk-2"))
	id, _ := newIdentity()
	rc := newReplayCache(replayWindowSec)
	raw, _ := sealEnvelope(k1, id, &syncMessage{Type: "announce"}, 1000)
	if _, _, err := openEnvelope(k2, raw, 1000, rc, replayWindowSec); err == nil {
		t.Fatal("wrong cluster key must fail to open (membership auth)")
	}
}
