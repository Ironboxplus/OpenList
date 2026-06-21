package cluster

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
	"sync"
)

// ----------------------------------------------------------------------------
// Sync groups (the multipartite mapping)
//
// A group links storages ACROSS nodes that should share one credential — e.g.
// the same 115 account mounted on cfscan, tx and home-nas. The set of groups is
// cluster-wide shared state, replicated as a single last-writer-wins document
// (admin edits are infrequent, so whole-doc LWW is the simplest convergent
// choice). Each group is a connected component of the overall k-partite graph.
// ----------------------------------------------------------------------------

// member is one storage on one node.
type member struct {
	NodeID    string `json:"node_id"`
	MountPath string `json:"mount_path"`
}

type group struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []member `json:"members"`
}

// mountsForNode returns the mount paths this node contributes to the group.
func (g group) mountsForNode(nodeID string) []string {
	var out []string
	for _, m := range g.Members {
		if m.NodeID == nodeID {
			out = append(out, m.MountPath)
		}
	}
	return out
}

// groupDoc is the replicated set of groups plus LWW version metadata.
type groupDoc struct {
	Groups    []group `json:"groups"`
	Version   uint64  `json:"version"`
	Origin    string  `json:"origin"`
	OriginPub []byte  `json:"origin_pub"`
	UpdatedAt int64   `json:"updated_at"`
	Sig       []byte  `json:"sig"`
}

// canonicalGroups returns the groups sorted deterministically so signing and
// comparison are stable regardless of insertion order.
func canonicalGroups(groups []group) []group {
	out := make([]group, len(groups))
	copy(out, groups)
	for i := range out {
		ms := make([]member, len(out[i].Members))
		copy(ms, out[i].Members)
		sort.Slice(ms, func(a, b int) bool {
			if ms[a].NodeID != ms[b].NodeID {
				return ms[a].NodeID < ms[b].NodeID
			}
			return ms[a].MountPath < ms[b].MountPath
		})
		out[i].Members = ms
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (d *groupDoc) signingBytes() []byte {
	canon := struct {
		Groups  []group `json:"groups"`
		Version uint64  `json:"version"`
		Origin  string  `json:"origin"`
	}{canonicalGroups(d.Groups), d.Version, d.Origin}
	b, _ := json.Marshal(canon)
	sum := sha256.Sum256(b)
	return sum[:]
}

func (d *groupDoc) verify() bool {
	if d.Version == 0 {
		return true // the empty/default doc is implicitly valid
	}
	if nodeIDFromPub(d.OriginPub) != d.Origin {
		return false
	}
	return verifySig(d.OriginPub, d.signingBytes(), d.Sig)
}

func (d *groupDoc) dominates(other *groupDoc) bool {
	if d.Version != other.Version {
		return d.Version > other.Version
	}
	return d.Origin > other.Origin
}

// ----------------------------------------------------------------------------
// Credential records (the thing actually replicated per group)
// ----------------------------------------------------------------------------

// credRecord carries a group's current credential payload. The payload holds the
// secret fields and only ever travels inside the AEAD-sealed envelope. Records
// are signed by their origin so they stay authentic across relays.
type credRecord struct {
	GroupID      string                     `json:"group_id"`
	OriginDriver string                     `json:"origin_driver"` // apply only to same-driver mounts
	Fields       []string                   `json:"fields"`        // credential field names included
	CredHash     string                     `json:"cred_hash"`     // idempotency key
	Payload      map[string]json.RawMessage `json:"payload"`       // field -> value (secret)
	Version      uint64                     `json:"version"`       // Lamport clock
	Origin       string                     `json:"origin"`        // authoring node id
	OriginPub    []byte                     `json:"origin_pub"`    // origin ed25519 pubkey
	OriginMount  string                     `json:"origin_mount"`  // informational
	UpdatedAt    int64                      `json:"updated_at"`
	Sig          []byte                     `json:"sig"`
}

func (r *credRecord) signingBytes() []byte {
	var b []byte
	b = append(b, r.GroupID...)
	b = append(b, 0)
	b = append(b, r.OriginDriver...)
	b = append(b, 0)
	b = append(b, r.CredHash...)
	b = append(b, 0)
	v := make([]byte, 8)
	for i := 0; i < 8; i++ {
		v[i] = byte(r.Version >> (8 * uint(i)))
	}
	b = append(b, v...)
	b = append(b, 0)
	b = append(b, r.Origin...)
	return b
}

func (r *credRecord) verify() bool {
	if nodeIDFromPub(r.OriginPub) != r.Origin {
		return false
	}
	if credHash(r.Payload) != r.CredHash {
		return false
	}
	return verifySig(r.OriginPub, r.signingBytes(), r.Sig)
}

func (r *credRecord) dominates(other *credRecord) bool {
	if r.Version != other.Version {
		return r.Version > other.Version
	}
	if r.Origin != other.Origin {
		return r.Origin > other.Origin
	}
	return r.CredHash > other.CredHash
}

// credDigest is the compact (no-secret) advert of a held credential record.
type credDigest struct {
	GroupID  string `json:"group_id"`
	CredHash string `json:"cred_hash"`
	Version  uint64 `json:"version"`
	Origin   string `json:"origin"`
}

func digestOfCred(r *credRecord) credDigest {
	return credDigest{GroupID: r.GroupID, CredHash: r.CredHash, Version: r.Version, Origin: r.Origin}
}

func credDigestDominates(a, b credDigest) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	if a.Origin != b.Origin {
		return a.Origin > b.Origin
	}
	return a.CredHash > b.CredHash
}

// ----------------------------------------------------------------------------
// Node inventory (soft state powering the UI + group editing)
// ----------------------------------------------------------------------------

type storageInfo struct {
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver"`
	Status    string `json:"status"`
}

type nodeInfo struct {
	NodeID    string        `json:"node_id"`
	Label     string        `json:"label"`
	Addr      string        `json:"addr"`
	Storages  []storageInfo `json:"storages"`
	Version   uint64        `json:"version"`
	UpdatedAt int64         `json:"updated_at"`
	// seenAt is set locally on receipt for liveness; not part of the wire form's
	// trust (it is overwritten each time we hear from/about the node).
	seenAt int64 `json:"-"`
}

// ----------------------------------------------------------------------------
// store: the in-memory cluster state (groups + creds + inventory)
// ----------------------------------------------------------------------------

type store struct {
	mu        sync.RWMutex
	groups    groupDoc
	creds     map[string]*credRecord // keyed by group id
	inventory map[string]*nodeInfo   // keyed by node id
	lamport   uint64
}

func newStore() *store {
	return &store{
		creds:     make(map[string]*credRecord),
		inventory: make(map[string]*nodeInfo),
	}
}

func (s *store) observe(v uint64) {
	if v > s.lamport {
		s.lamport = v
	}
}

// ---- groups ----

func (s *store) groupDoc() groupDoc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groups
}

func (s *store) groupList() []group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]group, len(s.groups.Groups))
	copy(out, s.groups.Groups)
	return out
}

// setGroups authors a new groups document locally (admin edit). Returns the
// signed doc to broadcast.
func (s *store) setGroups(id *identity, groups []group, now int64) groupDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lamport++
	d := groupDoc{
		Groups:    canonicalGroups(groups),
		Version:   s.lamport,
		Origin:    id.NodeID,
		OriginPub: id.Pub,
		UpdatedAt: now,
	}
	d.Sig = id.sign(d.signingBytes())
	s.groups = d
	return d
}

// mergeGroups integrates a peer's groups document under LWW. Returns true if it
// replaced ours.
func (s *store) mergeGroups(d *groupDoc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(d.Version)
	if d.dominates(&s.groups) {
		s.groups = *d
		return true
	}
	return false
}

// groupsForMount returns the groups that include a (this-node) mount path.
func (s *store) groupsForMount(nodeID, mountPath string) []group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []group
	for _, g := range s.groups.Groups {
		for _, m := range g.Members {
			if m.NodeID == nodeID && m.MountPath == mountPath {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func (s *store) groupByID(id string) (group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.groups.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return group{}, false
}

// ---- creds ----

func (s *store) getCred(groupID string) (*credRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.creds[groupID]
	return r, ok
}

func (s *store) credSnapshot() []*credRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*credRecord, 0, len(s.creds))
	for _, r := range s.creds {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out
}

func (s *store) credDigests() []credDigest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]credDigest, 0, len(s.creds))
	for _, r := range s.creds {
		out = append(out, digestOfCred(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out
}

// localCredChange records a credential a node observed on one of its own mounts.
// Returns (nil,false) when the credential is unchanged (idempotent: "其他节点看到
// 一样的 token 就不用变") or empty.
func (s *store) localCredChange(id *identity, groupID, driver, mount string, payload map[string]json.RawMessage, now int64) (*credRecord, bool) {
	h := credHash(payload)
	if h == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.creds[groupID]; ok && cur.CredHash == h {
		return nil, false // identical credential already known — no churn
	}
	s.lamport++
	r := &credRecord{
		GroupID:      groupID,
		OriginDriver: driver,
		Fields:       credFieldNames(payload),
		CredHash:     h,
		Payload:      payload,
		Version:      s.lamport,
		Origin:       id.NodeID,
		OriginPub:    id.Pub,
		OriginMount:  mount,
		UpdatedAt:    now,
	}
	r.Sig = id.sign(r.signingBytes())
	s.creds[groupID] = r
	return r, true
}

// mergeCred integrates a peer's credential record under LWW. The caller must have
// verified the signature. Returns true if it replaced/added ours.
func (s *store) mergeCred(r *credRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(r.Version)
	cur, ok := s.creds[r.GroupID]
	if ok {
		if cur.CredHash == r.CredHash {
			return false // idempotent: same credential
		}
		if !r.dominates(cur) {
			return false
		}
	}
	cp := *r
	s.creds[r.GroupID] = &cp
	return true
}

// pruneCreds drops credential records for groups that no longer exist.
func (s *store) pruneCreds() {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make(map[string]struct{}, len(s.groups.Groups))
	for _, g := range s.groups.Groups {
		live[g.ID] = struct{}{}
	}
	for id := range s.creds {
		if _, ok := live[id]; !ok {
			delete(s.creds, id)
		}
	}
}

// ---- inventory ----

func (s *store) setLocalInventory(info *nodeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info.seenAt = info.UpdatedAt
	s.inventory[info.NodeID] = info
}

// mergeInventory integrates a peer's inventory entry (LWW by version). seenAt is
// always refreshed so liveness reflects the latest contact.
func (s *store) mergeInventory(info *nodeInfo, now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.inventory[info.NodeID]
	if ok && info.Version < cur.Version {
		cur.seenAt = now // still heard about it; keep liveness fresh
		return false
	}
	cp := *info
	cp.seenAt = now
	s.inventory[info.NodeID] = &cp
	return true
}

func (s *store) inventoryList() []nodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]nodeInfo, 0, len(s.inventory))
	for _, n := range s.inventory {
		cp := *n
		cp.seenAt = n.seenAt
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// dialableAddrs returns advertised peer addresses (excluding our own node id) for
// auto-discovery dialing.
func (s *store) dialableAddrs(selfID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for id, n := range s.inventory {
		if id == selfID || n.Addr == "" {
			continue
		}
		out = append(out, n.Addr)
	}
	return out
}

// ---- persistence ----

type persistedState struct {
	Lamport   uint64        `json:"lamport"`
	Groups    groupDoc      `json:"groups"`
	Creds     []*credRecord `json:"creds"`
	Inventory []*nodeInfo   `json:"inventory"`
}

func (s *store) export() persistedState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps := persistedState{Lamport: s.lamport, Groups: s.groups}
	for _, r := range s.creds {
		ps.Creds = append(ps.Creds, r)
	}
	for _, n := range s.inventory {
		ps.Inventory = append(ps.Inventory, n)
	}
	return ps
}

func (s *store) load(ps persistedState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lamport = ps.Lamport
	s.groups = ps.Groups
	s.creds = make(map[string]*credRecord, len(ps.Creds))
	for _, r := range ps.Creds {
		s.creds[r.GroupID] = r
	}
	s.inventory = make(map[string]*nodeInfo, len(ps.Inventory))
	for _, n := range ps.Inventory {
		s.inventory[n.NodeID] = n
	}
}

func (s *store) lamportNow() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lamport
}

// shortHash returns a short prefix of a hash for display.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
