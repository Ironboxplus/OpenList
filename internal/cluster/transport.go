package cluster

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// syncMessage is the inner (encrypted) protocol payload exchanged between nodes.
// One message type carries push, anti-entropy announce, and pull in a single
// round trip:
//   - Records: full configs the sender is offering (push, or reply to a Want).
//   - Digests: compact (no-secrets) advert of everything the sender holds, so the
//     receiver can detect what it is missing.
//   - Wants:   mount paths the sender wants the receiver to send back in full.
type syncMessage struct {
	Type    string    `json:"type"` // "push" | "announce" | "pull" | "reply"
	Records []*record `json:"records,omitempty"`
	Digests []digest  `json:"digests,omitempty"`
	Wants   []string  `json:"wants,omitempty"`
}

// envelope is the outer, on-the-wire structure. Its Cipher is the syncMessage
// sealed with the cluster key; only PSK holders can open it (membership auth +
// confidentiality for the secrets inside). Sender/PubKey/Timestamp/Nonce are
// authenticated as AEAD additional data so they cannot be tampered with.
type envelope struct {
	Sender    string `json:"sender"` // sender node id (== hash(PubKey))
	PubKey    []byte `json:"pubkey"` // sender ed25519 pubkey
	Timestamp int64  `json:"ts"`     // unix seconds (replay window)
	Nonce     string `json:"nonce"`  // base64 random, replay cache key
	Cipher    []byte `json:"cipher"` // sealed syncMessage
}

// envelopeAAD binds the cleartext header fields to the ciphertext.
func envelopeAAD(sender string, pub []byte, ts int64, nonce string) []byte {
	aad := make([]byte, 0, len(sender)+len(pub)+len(nonce)+16)
	aad = append(aad, sender...)
	aad = append(aad, 0)
	aad = append(aad, pub...)
	aad = append(aad, 0)
	aad = append(aad, nonce...)
	aad = append(aad, 0)
	t := make([]byte, 8)
	for i := 0; i < 8; i++ {
		t[i] = byte(ts >> (8 * uint(i)))
	}
	aad = append(aad, t...)
	return aad
}

// sealEnvelope builds an encrypted envelope around a syncMessage.
func sealEnvelope(key []byte, id *identity, msg *syncMessage, now int64) ([]byte, error) {
	plain, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	nb := make([]byte, 18)
	if _, err := rand.Read(nb); err != nil {
		return nil, err
	}
	nonce := base64.RawStdEncoding.EncodeToString(nb)
	aad := envelopeAAD(id.NodeID, id.Pub, now, nonce)
	cipher, err := seal(key, plain, aad)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{
		Sender:    id.NodeID,
		PubKey:    id.Pub,
		Timestamp: now,
		Nonce:     nonce,
		Cipher:    cipher,
	})
}

// openEnvelope authenticates and decrypts an envelope. It enforces:
//   - sender node id == hash(pubkey) (self-consistent identity),
//   - timestamp within replayWindow of now,
//   - nonce not seen before (replay cache),
//   - AEAD open with the cluster key (membership + integrity + confidentiality).
func openEnvelope(key []byte, raw []byte, now int64, rc *replayCache, replayWindow int64) (*envelope, *syncMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, err
	}
	if nodeIDFromPub(env.PubKey) != env.Sender {
		return nil, nil, errors.New("sender id does not match pubkey")
	}
	if d := now - env.Timestamp; d > replayWindow || d < -replayWindow {
		return nil, nil, fmt.Errorf("timestamp outside replay window (%ds)", d)
	}
	if !rc.checkAndAdd(env.Nonce, now) {
		return nil, nil, errors.New("replayed nonce")
	}
	aad := envelopeAAD(env.Sender, env.PubKey, env.Timestamp, env.Nonce)
	plain, err := open(key, env.Cipher, aad)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt failed (wrong cluster key or tampered): %w", err)
	}
	var msg syncMessage
	if err := json.Unmarshal(plain, &msg); err != nil {
		return nil, nil, err
	}
	return &env, &msg, nil
}

// replayCache remembers recently-seen nonces to reject replays. Entries expire
// after the replay window so memory stays bounded.
type replayCache struct {
	mu     sync.Mutex
	seen   map[string]int64
	window int64
}

func newReplayCache(window int64) *replayCache {
	return &replayCache{seen: make(map[string]int64), window: window}
}

// checkAndAdd returns false if the nonce was already seen (a replay); otherwise
// it records the nonce and returns true. It opportunistically evicts expired
// entries.
func (rc *replayCache) checkAndAdd(nonce string, now int64) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, ok := rc.seen[nonce]; ok {
		return false
	}
	// Evict stale entries (cheap amortized sweep).
	if len(rc.seen) > 0 {
		for k, t := range rc.seen {
			if now-t > rc.window {
				delete(rc.seen, k)
			}
		}
	}
	rc.seen[nonce] = now
	return true
}
