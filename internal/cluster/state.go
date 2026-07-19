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
	if d == nil {
		return false
	}
	if d.Version == 0 {
		// Version zero is reserved for the exact local bootstrap document. A
		// non-empty v0 document has no authenticated ordering and must not win a
		// tie-break against a fresh node's empty state.
		return len(d.Groups) == 0 && d.Origin == "" && len(d.OriginPub) == 0 && len(d.Sig) == 0 && d.UpdatedAt == 0
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
	OriginMount  string                     `json:"origin_mount"`  // authoring mount path
	UpdatedAt    int64                      `json:"updated_at"`
	Sig          []byte                     `json:"sig"`                 // v1-compatible record signature
	MountSig     []byte                     `json:"mount_sig,omitempty"` // binds OriginMount for upgraded peers
}

// signingBytes deliberately preserves the original credential signature format.
// Older nodes ignore MountSig and can therefore keep verifying records from an
// upgraded node during a rolling deployment.
func (r *credRecord) signingBytes() []byte {
	return r.credentialSigningBytes(false)
}

// v2SigningBytes was briefly emitted by 5a61c4b5. It is accepted on read so a
// state.json written by that revision is not silently discarded.
func (r *credRecord) v2SigningBytes() []byte {
	return r.credentialSigningBytes(true)
}

func (r *credRecord) credentialSigningBytes(bindMount bool) []byte {
	var b []byte
	b = append(b, r.GroupID...)
	b = append(b, 0)
	b = append(b, r.OriginDriver...)
	b = append(b, 0)
	if bindMount {
		b = append(b, r.OriginMount...)
		b = append(b, 0)
	}
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

func (r *credRecord) mountSigningBytes() []byte {
	b := append([]byte("openlist/cluster/credential-mount/v1\x00"), r.signingBytes()...)
	b = append(b, 0)
	b = append(b, r.OriginMount...)
	return b
}

type credentialSignature uint8

const (
	credentialSignatureInvalid credentialSignature = iota
	credentialSignatureLegacy
	credentialSignatureMountBound
	credentialSignatureV2
)

func (r *credRecord) signatureKind() credentialSignature {
	if r == nil || nodeIDFromPub(r.OriginPub) != r.Origin || credHash(r.Payload) != r.CredHash {
		return credentialSignatureInvalid
	}
	if verifySig(r.OriginPub, r.signingBytes(), r.Sig) {
		if len(r.MountSig) == 0 {
			return credentialSignatureLegacy
		}
		if verifySig(r.OriginPub, r.mountSigningBytes(), r.MountSig) {
			return credentialSignatureMountBound
		}
		return credentialSignatureInvalid
	}
	if len(r.MountSig) == 0 && verifySig(r.OriginPub, r.v2SigningBytes(), r.Sig) {
		return credentialSignatureV2
	}
	return credentialSignatureInvalid
}

func (r *credRecord) verify() bool {
	return r.signatureKind() != credentialSignatureInvalid
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

// credRevocation is a signed tombstone for one source mount's credential
// candidate. It is intentionally separate from a credential record: a relay
// must retain and forward the tombstone after it has discarded the secret, or
// an old signed credential can be replayed back to its source.
type credRevocation struct {
	GroupID     string `json:"group_id"`
	Origin      string `json:"origin"`
	OriginPub   []byte `json:"origin_pub"`
	OriginMount string `json:"origin_mount"`
	CredHash    string `json:"cred_hash"`
	Version     uint64 `json:"version"`
	UpdatedAt   int64  `json:"updated_at"`
	Sig         []byte `json:"sig"`
}

func (r *credRevocation) signingBytes() []byte {
	var b []byte
	b = append(b, "openlist/cluster/credential-revocation/v1\x00"...)
	b = append(b, r.GroupID...)
	b = append(b, 0)
	b = append(b, r.OriginMount...)
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

func (r *credRevocation) verify() bool {
	return r != nil && r.GroupID != "" && r.OriginMount != "" && r.CredHash != "" &&
		r.Version > 0 && nodeIDFromPub(r.OriginPub) == r.Origin &&
		verifySig(r.OriginPub, r.signingBytes(), r.Sig)
}

func (r *credRevocation) dominates(other *credRevocation) bool {
	if other == nil || r.Version != other.Version {
		return other == nil || r.Version > other.Version
	}
	return r.CredHash > other.CredHash
}

// credDigest is the compact (no-secret) advert of a held credential record.
type credDigest struct {
	GroupID     string `json:"group_id"`
	CredHash    string `json:"cred_hash"`
	Version     uint64 `json:"version"`
	Origin      string `json:"origin"`
	OriginMount string `json:"origin_mount,omitempty"`
}

func digestOfCred(r *credRecord) credDigest {
	return credDigest{GroupID: r.GroupID, CredHash: r.CredHash, Version: r.Version, Origin: r.Origin, OriginMount: r.OriginMount}
}

func candidateKey(origin, mount string) string { return origin + "\x00" + mount }

// credDigestKey identifies one mount-level candidate within a group.
func credDigestKey(d credDigest) string {
	return d.GroupID + "\x00" + candidateKey(d.Origin, d.OriginMount)
}

func credDigestDominates(a, b credDigest) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	if a.Origin != b.Origin {
		return a.Origin > b.Origin
	}
	if a.OriginMount != b.OriginMount {
		return a.OriginMount > b.OriginMount
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
	mu     sync.RWMutex
	groups groupDoc
	// One group is a peer set, not a primary/replica pair. Each member keeps its
	// own current candidate, keyed by the signing origin and mount; Lamport
	// ordering only resolves successive credentials from the same source mount.
	creds map[string]map[string]*credRecord // group id -> (origin, mount) -> candidate
	// revocations are source-signed tombstones. They prevent a relay from
	// reintroducing a credential after that source proved it invalid.
	revocations map[string]map[string]*credRevocation // group id -> (origin, mount) -> tombstone
	// failedCandidates is node-local only: a candidate may be valid on its origin
	// yet unusable from this node. Persisting the quarantine avoids a restart
	// repeatedly refreshing the exact token that just failed here.
	failedCandidates map[string]map[string]map[string]int64 // group id -> local mount -> credential hash -> failed unix second
	// candidateHealth is an in-memory proof that this node has actually used a
	// credential successfully. It is deliberately not persisted: a restart must
	// prove the credential again instead of trusting a stale WORK status.
	candidateHealth map[string]map[string]int64 // group id -> credential hash -> last proven unix second
	inventory       map[string]*nodeInfo        // keyed by node id
	lamport         uint64
}

const candidateLeaseSec int64 = 15 * 60

func newStore() *store {
	return &store{
		creds:            make(map[string]map[string]*credRecord),
		revocations:      make(map[string]map[string]*credRevocation),
		failedCandidates: make(map[string]map[string]map[string]int64),
		candidateHealth:  make(map[string]map[string]int64),
		inventory:        make(map[string]*nodeInfo),
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

// credentialAuthorizedByGroups binds a signed credential to a configured source
// mount. Legacy signatures did not bind OriginMount, so they are safe only when
// that node has exactly one member mount in the group; upgraded records carry a
// separate MountSig and support multiple mounts from the same node.
func credentialAuthorizedByGroups(groups []group, r *credRecord) bool {
	if r == nil || r.GroupID == "" || r.Origin == "" || r.OriginMount == "" || !r.verify() {
		return false
	}
	var membersForOrigin int
	var exactMount bool
	for _, g := range groups {
		if g.ID != r.GroupID {
			continue
		}
		for _, member := range g.Members {
			if member.NodeID != r.Origin {
				continue
			}
			membersForOrigin++
			if member.MountPath == r.OriginMount {
				exactMount = true
			}
		}
	}
	if !exactMount {
		return false
	}
	return r.signatureKind() != credentialSignatureLegacy || membersForOrigin == 1
}

func credentialRevocationAuthorizedByGroups(groups []group, rev *credRevocation) bool {
	if !rev.verify() {
		return false
	}
	for _, g := range groups {
		if g.ID != rev.GroupID {
			continue
		}
		for _, member := range g.Members {
			if member.NodeID == rev.Origin && member.MountPath == rev.OriginMount {
				return true
			}
		}
	}
	return false
}

// ---- creds ----

func (s *store) getCred(groupID string) (*credRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var newest *credRecord
	for _, r := range s.creds[groupID] {
		if newest == nil || r.dominates(newest) {
			newest = r
		}
	}
	return newest, newest != nil
}

func (s *store) getCredForSource(groupID, origin, mount string) (*credRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.creds[groupID][candidateKey(origin, mount)]
	return r, ok
}

func (s *store) credsForGroup(groupID string) []*credRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byOrigin := s.creds[groupID]
	out := make([]*credRecord, 0, len(byOrigin))
	for _, r := range byOrigin {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].OriginMount < out[j].OriginMount
	})
	return out
}

func (s *store) hasCredHash(groupID, hash string) bool {
	if hash == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.creds[groupID] {
		if r.CredHash == hash {
			return true
		}
	}
	return false
}

func (s *store) hasCredForSource(groupID, origin, mount, hash string) bool {
	if hash == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.creds[groupID][candidateKey(origin, mount)]
	return ok && r.CredHash == hash
}

func (s *store) credSnapshot() []*credRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*credRecord
	for _, byOrigin := range s.creds {
		for _, r := range byOrigin {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].OriginMount < out[j].OriginMount
	})
	return out
}

func (s *store) credDigests() []credDigest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []credDigest
	for _, byOrigin := range s.creds {
		for _, r := range byOrigin {
			out = append(out, digestOfCred(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].OriginMount < out[j].OriginMount
	})
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
	byOrigin := s.creds[groupID]
	if byOrigin == nil {
		byOrigin = make(map[string]*credRecord)
		s.creds[groupID] = byOrigin
	}
	if cur, ok := byOrigin[candidateKey(id.NodeID, mount)]; ok && cur.CredHash == h && cur.signatureKind() == credentialSignatureMountBound {
		return nil, false // this source mount has not changed
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
	r.MountSig = id.sign(r.mountSigningBytes())
	byOrigin[candidateKey(id.NodeID, mount)] = r
	return r, true
}

// mergeCred integrates a peer's credential record under LWW. The caller must have
// verified the signature. Returns true if it replaced/added ours.
func (s *store) mergeCred(r *credRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(r.Version)
	byOrigin := s.creds[r.GroupID]
	if byOrigin == nil {
		byOrigin = make(map[string]*credRecord)
		s.creds[r.GroupID] = byOrigin
	}
	key := candidateKey(r.Origin, r.OriginMount)
	if rev := s.revocations[r.GroupID][key]; rev != nil && rev.Version >= r.Version {
		return false // a source-signed tombstone blocks relay replay of this version
	}
	cur, ok := byOrigin[key]
	if ok {
		if cur.CredHash == r.CredHash {
			return false // idempotent: same credential
		}
		if !r.dominates(cur) {
			return false
		}
	}
	cp := *r
	byOrigin[key] = &cp
	return true
}

// dropOwnCred removes this source mount's local candidate without revoking it.
// It is used for uncertain startup failures (for example a transient TLS
// timeout), where this node must stop advertising the candidate but must not
// globally invalidate a credential that may still work on its origin.
func (s *store) dropOwnCred(groupID, selfID, mount string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	byOrigin := s.creds[groupID]
	if byOrigin == nil {
		return false
	}
	key := candidateKey(selfID, mount)
	if _, ok := byOrigin[key]; !ok {
		return false
	}
	delete(byOrigin, key)
	if len(byOrigin) == 0 {
		delete(s.creds, groupID)
	}
	return true
}

// revokeOwnCred removes a source candidate only after the provider has
// explicitly declared it invalid. Unlike dropOwnCred, this emits a signed
// tombstone so relays cannot replay the candidate back to its origin.
func (s *store) revokeOwnCred(id *identity, groupID, mount string, at int64) (*credRevocation, bool) {
	if id == nil || at <= 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byOrigin := s.creds[groupID]
	if byOrigin == nil {
		return nil, false
	}
	key := candidateKey(id.NodeID, mount)
	current := byOrigin[key]
	if current == nil {
		return nil, false
	}
	s.lamport++
	rev := &credRevocation{
		GroupID:     groupID,
		Origin:      id.NodeID,
		OriginPub:   id.Pub,
		OriginMount: mount,
		CredHash:    current.CredHash,
		Version:     s.lamport,
		UpdatedAt:   at,
	}
	rev.Sig = id.sign(rev.signingBytes())
	byRevocation := s.revocations[groupID]
	if byRevocation == nil {
		byRevocation = make(map[string]*credRevocation)
		s.revocations[groupID] = byRevocation
	}
	byRevocation[key] = rev
	delete(byOrigin, key)
	if len(byOrigin) == 0 {
		delete(s.creds, groupID)
	}
	cp := *rev
	return &cp, true
}

func (s *store) revocationSnapshot() []*credRevocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*credRevocation
	for _, bySource := range s.revocations {
		for _, rev := range bySource {
			if !rev.verify() {
				continue
			}
			cp := *rev
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].OriginMount < out[j].OriginMount
	})
	return out
}

// mergeRevocation applies a verified tombstone and removes any same-source
// candidate that it supersedes. A later credential from the source remains
// allowed because it must carry a strictly greater Lamport version.
func (s *store) mergeRevocation(rev *credRevocation) bool {
	if rev == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(rev.Version)
	key := candidateKey(rev.Origin, rev.OriginMount)
	byRevocation := s.revocations[rev.GroupID]
	if byRevocation == nil {
		byRevocation = make(map[string]*credRevocation)
		s.revocations[rev.GroupID] = byRevocation
	}
	if current := byRevocation[key]; current != nil && !rev.dominates(current) {
		return false
	}
	cp := *rev
	byRevocation[key] = &cp
	if byOrigin := s.creds[rev.GroupID]; byOrigin != nil {
		if current := byOrigin[key]; current != nil && current.Version <= rev.Version {
			delete(byOrigin, key)
			if len(byOrigin) == 0 {
				delete(s.creds, rev.GroupID)
			}
		}
	}
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
			delete(s.candidateHealth, id)
		}
	}
	for id := range s.revocations {
		if _, ok := live[id]; !ok {
			delete(s.revocations, id)
		}
	}
	for id := range s.failedCandidates {
		if _, ok := live[id]; !ok {
			delete(s.failedCandidates, id)
		}
	}
}

func (s *store) markCandidateHealthy(groupID, hash string, at int64) {
	if groupID == "" || hash == "" || at <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byHash := s.candidateHealth[groupID]
	if byHash == nil {
		byHash = make(map[string]int64)
		s.candidateHealth[groupID] = byHash
	}
	byHash[hash] = at
}

func (s *store) forgetCandidateHealth(groupID, hash string) {
	if groupID == "" || hash == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byHash := s.candidateHealth[groupID]
	delete(byHash, hash)
	if len(byHash) == 0 {
		delete(s.candidateHealth, groupID)
	}
}

func (s *store) candidateHealthy(groupID, hash string, at int64) bool {
	if groupID == "" || hash == "" || at <= 0 {
		return false
	}
	s.mu.RLock()
	provenAt := s.candidateHealth[groupID][hash]
	s.mu.RUnlock()
	return provenAt > 0 && at >= provenAt && at-provenAt <= candidateLeaseSec
}

func (s *store) markCandidateFailed(groupID, mount, hash string, at int64) {
	if groupID == "" || mount == "" || hash == "" || at <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byMount := s.failedCandidates[groupID]
	if byMount == nil {
		byMount = make(map[string]map[string]int64)
		s.failedCandidates[groupID] = byMount
	}
	byHash := byMount[mount]
	if byHash == nil {
		byHash = make(map[string]int64)
		byMount[mount] = byHash
	}
	byHash[hash] = at
}

func (s *store) clearCandidateFailed(groupID, mount, hash string) {
	if groupID == "" || mount == "" || hash == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byMount := s.failedCandidates[groupID]
	byHash := byMount[mount]
	delete(byHash, hash)
	if len(byHash) == 0 {
		delete(byMount, mount)
	}
	if len(byMount) == 0 {
		delete(s.failedCandidates, groupID)
	}
}

func (s *store) candidateFailed(groupID, mount, hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.failedCandidates[groupID][mount][hash] > 0
}

// recoveryCandidates returns persisted alternatives for one local mount. The
// current failed credential and any locally quarantined candidate are excluded;
// the order is deterministic so a recovery attempt is bounded and reproducible.
func (s *store) recoveryCandidates(groupID, mount, failedHash string) []*credRecord {
	candidates := s.credsForGroup(groupID)
	out := make([]*credRecord, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.CredHash == failedHash || s.candidateFailed(groupID, mount, candidate.CredHash) {
			continue
		}
		out = append(out, candidate)
	}
	return out
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
	Lamport          uint64             `json:"lamport"`
	Groups           groupDoc           `json:"groups"`
	Creds            []*credRecord      `json:"creds"`
	Revocations      []*credRevocation  `json:"revocations,omitempty"`
	FailedCandidates []candidateFailure `json:"failed_candidates,omitempty"`
	Inventory        []*nodeInfo        `json:"inventory"`
}

// candidateFailure is local-only recovery metadata; it deliberately contains no
// credential payload, only the already-public credential hash.
type candidateFailure struct {
	GroupID  string `json:"group_id"`
	Mount    string `json:"mount"`
	CredHash string `json:"cred_hash"`
	FailedAt int64  `json:"failed_at"`
}

func (s *store) export() persistedState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps := persistedState{Lamport: s.lamport, Groups: s.groups}
	for _, byOrigin := range s.creds {
		for _, r := range byOrigin {
			ps.Creds = append(ps.Creds, r)
		}
	}
	for _, bySource := range s.revocations {
		for _, rev := range bySource {
			if rev.verify() {
				ps.Revocations = append(ps.Revocations, rev)
			}
		}
	}
	for groupID, byMount := range s.failedCandidates {
		for mount, byHash := range byMount {
			for hash, failedAt := range byHash {
				ps.FailedCandidates = append(ps.FailedCandidates, candidateFailure{
					GroupID: groupID, Mount: mount, CredHash: hash, FailedAt: failedAt,
				})
			}
		}
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
	if ps.Groups.verify() {
		s.groups = ps.Groups
	} else {
		s.groups = groupDoc{}
	}
	s.creds = make(map[string]map[string]*credRecord)
	s.revocations = make(map[string]map[string]*credRevocation)
	s.candidateHealth = make(map[string]map[string]int64)
	s.failedCandidates = make(map[string]map[string]map[string]int64)
	for _, rev := range ps.Revocations {
		if !credentialRevocationAuthorizedByGroups(s.groups.Groups, rev) {
			continue
		}
		bySource := s.revocations[rev.GroupID]
		if bySource == nil {
			bySource = make(map[string]*credRevocation)
			s.revocations[rev.GroupID] = bySource
		}
		key := candidateKey(rev.Origin, rev.OriginMount)
		if current := bySource[key]; current == nil || rev.dominates(current) {
			cp := *rev
			bySource[key] = &cp
		}
		if rev.Version > s.lamport {
			s.lamport = rev.Version
		}
	}
	for _, r := range ps.Creds {
		if !credentialAuthorizedByGroups(s.groups.Groups, r) {
			continue
		}
		key := candidateKey(r.Origin, r.OriginMount)
		if rev := s.revocations[r.GroupID][key]; rev != nil && rev.Version >= r.Version {
			continue
		}
		byOrigin := s.creds[r.GroupID]
		if byOrigin == nil {
			byOrigin = make(map[string]*credRecord)
			s.creds[r.GroupID] = byOrigin
		}
		if cur, ok := byOrigin[key]; !ok || r.dominates(cur) {
			cp := *r
			byOrigin[key] = &cp
		}
		if r.Version > s.lamport {
			s.lamport = r.Version
		}
	}
	for _, failed := range ps.FailedCandidates {
		if failed.GroupID == "" || failed.Mount == "" || failed.CredHash == "" || failed.FailedAt <= 0 {
			continue
		}
		byMount := s.failedCandidates[failed.GroupID]
		if byMount == nil {
			byMount = make(map[string]map[string]int64)
			s.failedCandidates[failed.GroupID] = byMount
		}
		byHash := byMount[failed.Mount]
		if byHash == nil {
			byHash = make(map[string]int64)
			byMount[failed.Mount] = byHash
		}
		if failed.FailedAt > byHash[failed.CredHash] {
			byHash[failed.CredHash] = failed.FailedAt
		}
	}
	s.inventory = make(map[string]*nodeInfo, len(ps.Inventory))
	for _, n := range ps.Inventory {
		if n == nil || n.NodeID == "" {
			continue
		}
		cp := *n
		s.inventory[n.NodeID] = &cp
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
