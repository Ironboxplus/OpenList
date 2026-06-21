package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// syncableStorage is the canonical, node-independent view of a storage mount that
// we replicate between cluster nodes. Node-local runtime fields (ID, Status,
// Modified) are deliberately excluded so the content hash is identical on every
// node for the same logical config — that idempotency is what lets a node ignore
// a peer that reports "the same token I already have" ("其他节点看到一样的token就不用变").
type syncableStorage struct {
	MountPath           string `json:"mount_path"`
	Order               int    `json:"order"`
	Driver              string `json:"driver"`
	CacheExpiration     int    `json:"cache_expiration"`
	CustomCachePolicies string `json:"custom_cache_policies"`
	Addition            string `json:"addition"`
	Remark              string `json:"remark"`
	Disabled            bool   `json:"disabled"`
	DisableIndex        bool   `json:"disable_index"`
	EnableSign          bool   `json:"enable_sign"`
	// Sort
	OrderBy        string `json:"order_by"`
	OrderDirection string `json:"order_direction"`
	ExtractFolder  string `json:"extract_folder"`
	// Proxy
	WebProxy         bool   `json:"web_proxy"`
	WebdavPolicy     string `json:"webdav_policy"`
	ProxyRange       bool   `json:"proxy_range"`
	DownProxyURL     string `json:"down_proxy_url"`
	DisableProxySign bool   `json:"disable_proxy_sign"`
}

func fromModel(s *model.Storage) syncableStorage {
	return syncableStorage{
		MountPath:           s.MountPath,
		Order:               s.Order,
		Driver:              s.Driver,
		CacheExpiration:     s.CacheExpiration,
		CustomCachePolicies: s.CustomCachePolicies,
		Addition:            s.Addition,
		Remark:              s.Remark,
		Disabled:            s.Disabled,
		DisableIndex:        s.DisableIndex,
		EnableSign:          s.EnableSign,
		OrderBy:             s.OrderBy,
		OrderDirection:      s.OrderDirection,
		ExtractFolder:       s.ExtractFolder,
		WebProxy:            s.WebProxy,
		WebdavPolicy:        s.WebdavPolicy,
		ProxyRange:          s.ProxyRange,
		DownProxyURL:        s.DownProxyURL,
		DisableProxySign:    s.DisableProxySign,
	}
}

// applyTo writes the synced fields onto an existing storage model, preserving the
// node-local runtime fields (ID/Status/Modified) of dst.
func (sc syncableStorage) applyTo(dst *model.Storage) {
	dst.MountPath = sc.MountPath
	dst.Order = sc.Order
	dst.Driver = sc.Driver
	dst.CacheExpiration = sc.CacheExpiration
	dst.CustomCachePolicies = sc.CustomCachePolicies
	dst.Addition = sc.Addition
	dst.Remark = sc.Remark
	dst.Disabled = sc.Disabled
	dst.DisableIndex = sc.DisableIndex
	dst.EnableSign = sc.EnableSign
	dst.OrderBy = sc.OrderBy
	dst.OrderDirection = sc.OrderDirection
	dst.ExtractFolder = sc.ExtractFolder
	dst.WebProxy = sc.WebProxy
	dst.WebdavPolicy = sc.WebdavPolicy
	dst.ProxyRange = sc.ProxyRange
	dst.DownProxyURL = sc.DownProxyURL
	dst.DisableProxySign = sc.DisableProxySign
}

// canonicalJSON marshals deterministically. encoding/json already sorts struct
// fields by declaration order (stable), so a plain Marshal is canonical here.
func (sc syncableStorage) canonicalJSON() []byte {
	b, _ := json.Marshal(sc)
	return b
}

// contentHash is the idempotency key: identical config → identical hash on every
// node, regardless of who authored it.
func (sc syncableStorage) contentHash() string {
	sum := sha256.Sum256(sc.canonicalJSON())
	return hex.EncodeToString(sum[:])
}

// record is one replicated mount entry: the config plus CRDT version metadata.
// Ordering is a Lamport clock with the origin node id as a deterministic
// tiebreaker, giving a total order for last-writer-wins convergence.
type record struct {
	MountPath   string          `json:"mount_path"`
	ContentHash string          `json:"content_hash"`
	Version     uint64          `json:"version"`    // Lamport logical clock
	Origin      string          `json:"origin"`     // node id that authored this version
	OriginPub   []byte          `json:"origin_pub"` // origin's ed25519 pubkey; node id == hash(pubkey)
	Tombstone   bool            `json:"tombstone"`  // true => mount was deleted
	UpdatedAt   int64           `json:"updated_at"` // unix seconds, informational only
	Config      syncableStorage `json:"config"`     // empty when Tombstone
	Sig         []byte          `json:"sig"`        // origin's ed25519 signature over signingBytes
}

// verify checks a record is internally authentic: the origin node id is the hash
// of the embedded pubkey (so a member cannot claim another node's id without its
// private key), the signature is valid under that pubkey, and the content hash
// matches the carried config. The cluster PSK (transport seal) gates membership;
// this gates per-record origin integrity for relayed records.
func (r *record) verify() bool {
	if nodeIDFromPub(r.OriginPub) != r.Origin {
		return false
	}
	if !r.Tombstone {
		if r.Config.contentHash() != r.ContentHash {
			return false
		}
	} else if r.ContentHash != (syncableStorage{MountPath: r.MountPath}).contentHash() {
		return false
	}
	return verifySig(r.OriginPub, r.signingBytes(), r.Sig)
}

// signingBytes is the stable byte string an origin node signs to authenticate a
// version. It excludes the signature itself and the (informational) timestamp.
func (r *record) signingBytes() []byte {
	// length-free, delimiter-joined fields; ContentHash already binds Config.
	var b []byte
	b = append(b, r.MountPath...)
	b = append(b, 0)
	b = append(b, r.ContentHash...)
	b = append(b, 0)
	v := make([]byte, 8)
	for i := 0; i < 8; i++ {
		v[i] = byte(r.Version >> (8 * uint(i)))
	}
	b = append(b, v...)
	b = append(b, 0)
	b = append(b, r.Origin...)
	b = append(b, 0)
	if r.Tombstone {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	return b
}

// dominates reports whether r should win over other under LWW ordering.
func (r *record) dominates(other *record) bool {
	if r.Version != other.Version {
		return r.Version > other.Version
	}
	// Equal Lamport time: break ties deterministically by origin id, then by
	// content hash so two distinct concurrent edits still converge identically
	// on every node.
	if r.Origin != other.Origin {
		return r.Origin > other.Origin
	}
	return r.ContentHash > other.ContentHash
}

// digest is the compact form announced to peers so they can detect divergence
// without shipping full configs (and secrets) on every heartbeat.
type digest struct {
	MountPath   string `json:"mount_path"`
	ContentHash string `json:"content_hash"`
	Version     uint64 `json:"version"`
	Origin      string `json:"origin"`
	Tombstone   bool   `json:"tombstone"`
}

// store is the in-memory CRDT keyed by mount path, with a monotonically
// non-decreasing Lamport clock shared across all mounts.
type store struct {
	mu      sync.RWMutex
	records map[string]*record
	lamport uint64
}

func newStore() *store {
	return &store{records: make(map[string]*record)}
}

// tick advances and returns the Lamport clock for a locally-originated change.
func (s *store) tick() uint64 {
	s.lamport++
	return s.lamport
}

// observe bumps the Lamport clock to stay ahead of a value seen from a peer.
func (s *store) observe(v uint64) {
	if v > s.lamport {
		s.lamport = v
	}
}

func (s *store) get(mountPath string) (*record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[mountPath]
	return r, ok
}

func (s *store) snapshot() []*record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPath < out[j].MountPath })
	return out
}

func (s *store) digests() []digest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]digest, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, digest{
			MountPath:   r.MountPath,
			ContentHash: r.ContentHash,
			Version:     r.Version,
			Origin:      r.Origin,
			Tombstone:   r.Tombstone,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPath < out[j].MountPath })
	return out
}

// localChange records a config a node authored itself. It returns the new record
// to broadcast, or (nil,false) when nothing changed (idempotent no-op): same
// content hash and not resurrecting a tombstone.
func (s *store) localChange(id *identity, cfg syncableStorage, now int64) (*record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := cfg.contentHash()
	if cur, ok := s.records[cfg.MountPath]; ok && !cur.Tombstone && cur.ContentHash == h {
		return nil, false // unchanged — do not bump version or churn peers
	}
	s.lamport++
	r := &record{
		MountPath:   cfg.MountPath,
		ContentHash: h,
		Version:     s.lamport,
		Origin:      id.NodeID,
		OriginPub:   id.Pub,
		Tombstone:   false,
		UpdatedAt:   now,
		Config:      cfg,
	}
	r.Sig = id.sign(r.signingBytes())
	s.records[cfg.MountPath] = r
	return r, true
}

// localDelete authors a tombstone for a mount. Returns (nil,false) if already
// tombstoned or unknown.
func (s *store) localDelete(id *identity, mountPath string, now int64) (*record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.records[mountPath]
	if !ok || cur.Tombstone {
		return nil, false
	}
	s.lamport++
	r := &record{
		MountPath: mountPath,
		// hash of an empty config keeps signingBytes well-defined for tombstones
		ContentHash: syncableStorage{MountPath: mountPath}.contentHash(),
		Version:     s.lamport,
		Origin:      id.NodeID,
		OriginPub:   id.Pub,
		Tombstone:   true,
		UpdatedAt:   now,
	}
	r.Sig = id.sign(r.signingBytes())
	s.records[mountPath] = r
	return r, true
}

// mergeResult describes what merge did with an incoming record.
type mergeResult int

const (
	mergeIgnored  mergeResult = iota // incoming did not win (older/equal/duplicate)
	mergeApplied                     // incoming won and replaced local (config changed)
	mergeTombstone                   // incoming won and is a delete
)

// merge integrates a peer's record. The caller must have already verified r.Sig
// against the origin's known public key. merge enforces LWW ordering and the
// idempotency rule, and advances the Lamport clock.
func (s *store) merge(r *record) mergeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Version > s.lamport {
		s.lamport = r.Version
	}
	cur, ok := s.records[r.MountPath]
	if ok {
		// Idempotent: identical live content, ignore regardless of version churn.
		if !r.Tombstone && !cur.Tombstone && cur.ContentHash == r.ContentHash {
			return mergeIgnored
		}
		if !r.dominates(cur) {
			return mergeIgnored
		}
	}
	cp := *r
	s.records[r.MountPath] = &cp
	if cp.Tombstone {
		return mergeTombstone
	}
	return mergeApplied
}

// load replaces the store contents from a persisted snapshot.
func (s *store) load(records []*record, lamport uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[string]*record, len(records))
	for _, r := range records {
		s.records[r.MountPath] = r
	}
	s.lamport = lamport
}

func (s *store) lamportNow() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lamport
}
