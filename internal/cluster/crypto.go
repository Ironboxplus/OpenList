package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
)

// Cryptographic design (see docs/cluster-sync.md):
//
//   - A cluster pre-shared key (PSK) is the root of trust. HKDF-SHA256 expands it
//     into an AES-256-GCM session key. Only nodes holding the PSK can seal/open
//     envelopes, which both authenticates membership and encrypts the payload —
//     essential because storage configs carry secrets (cloud tokens/passwords).
//   - Replay is prevented by a per-envelope random nonce plus a timestamp window
//     and a recently-seen-nonce cache (enforced by the transport layer).
//   - Each node owns an ed25519 identity keypair. Every synced config version is
//     signed by its origin node, so a malicious member cannot forge a higher
//     version for a mount it doesn't own (CRDT integrity), and nodes can be
//     revoked by dropping their public key.
const (
	aeadInfo  = "openlist-cluster-aead-v1"
	aeadKeyN  = 32 // AES-256
	nodeIDLen = 16 // bytes of the pubkey hash encoded into the node id
)

// deriveAEADKey expands the cluster PSK into a stable AES-256 key.
func deriveAEADKey(psk []byte) ([]byte, error) {
	if len(psk) == 0 {
		return nil, errors.New("empty cluster key")
	}
	// Fixed salt: the PSK is the secret; a per-cluster constant salt is fine and
	// keeps key derivation deterministic across nodes.
	return hkdf.Key(sha256.New, psk, []byte("openlist-cluster"), aeadInfo, aeadKeyN)
}

// aead builds the AES-256-GCM AEAD from a derived key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts plaintext with the cluster key, binding aad. Output is
// nonce||ciphertext. aad is authenticated but not encrypted.
func seal(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// open reverses seal. It fails (returns error) if the key is wrong, the data was
// tampered with, or aad does not match — i.e. it both decrypts and authenticates.
func open(key, blob, aad []byte) ([]byte, error) {
	gcm, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return gcm.Open(nil, nonce, ct, aad)
}

// identity is a node's ed25519 keypair plus its derived node id.
type identity struct {
	NodeID string
	Pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
}

// newIdentity generates a fresh ed25519 identity.
func newIdentity() (*identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &identity{NodeID: nodeIDFromPub(pub), Pub: pub, priv: priv}, nil
}

// identityFromSeed rebuilds an identity from a persisted private-key seed.
func identityFromSeed(seed []byte) (*identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("bad identity seed length %d", len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &identity{NodeID: nodeIDFromPub(pub), Pub: pub, priv: priv}, nil
}

func (id *identity) seed() []byte { return id.priv.Seed() }

func (id *identity) sign(msg []byte) []byte { return ed25519.Sign(id.priv, msg) }

// nodeIDFromPub derives a short, stable, human-friendly node id from a pubkey.
func nodeIDFromPub(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:nodeIDLen])
}

// verifySig checks an ed25519 signature against a known public key.
func verifySig(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
