package cluster

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
)

// loadOrCreateIdentity returns this node's stable ed25519 identity, generating
// and persisting a fresh one (the 32-byte seed) on first run. The seed file is
// 0600 — it is the node's private key and the basis of its node id, which other
// nodes pin (a node id is the hash of the public key).
func loadOrCreateIdentity(dir string) (*identity, error) {
	path := filepath.Join(dir, "identity.key")
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		return identityFromSeed(seed)
	}
	id, err := newIdentity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, id.seed(), 0o600); err != nil {
		return nil, err
	}
	return id, nil
}
