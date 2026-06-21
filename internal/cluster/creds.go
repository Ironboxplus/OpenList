package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// This file isolates the one thing the cluster actually replicates: a storage's
// CREDENTIAL fields. The user requirement is explicit — "除了 key 其他的都不用同步"
// (sync only the key/credential, nothing else). So instead of shipping a whole
// storage config we extract just the secret-bearing subset of a driver's Addition
// JSON, propagate that, and overlay it onto the matching storage on peer nodes,
// leaving every node-local field (mount path, root folder, cache, order, proxy…)
// untouched.
//
// OpenList drivers do not tag credential fields, so we identify them by name. The
// keyword set covers the credential fields used across drivers (refresh_token,
// access_token, cookie, password, *_secret, api_key, authorization, …). Matching
// is done on a normalized (lowercased, separator-stripped) field name so
// "refresh_token", "RefreshToken" and "refreshToken" all match. The match runs
// only over Addition keys, where non-credential fields are structural
// (root_folder_id, order_by, …) and never contain these tokens — so false
// positives are not a concern in practice.
var credKeywords = []string{
	"token",      // access_token, refresh_token, token
	"cookie",     // cookie / cookies
	"password",   // password
	"passwd",     // passwd
	"secret",     // client_secret, app_secret, secret_key
	"auth",       // authorization, auth
	"session",    // session, session_id
	"refresh",    // refresh_token (redundant w/ token, kept for clarity)
	"access",     // access_token (redundant w/ token)
	"credential", // credential / credentials
	"apikey",     // api_key, apikey
	"appkey",     // app_key
	"privatekey", // private_key
	"signkey",    // sign_key
	"ticket",     // some drivers use a login ticket
	"passport",   // 123pan-style passport secret
}

// normalizeKey lowercases and strips separators so "Refresh_Token" == "refreshtoken".
func normalizeKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			// drop '_', '-', spaces, etc.
		}
	}
	return b.String()
}

// isCredField reports whether an Addition field name denotes a credential.
func isCredField(name string) bool {
	n := normalizeKey(name)
	if n == "" {
		return false
	}
	for _, k := range credKeywords {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// extractCreds parses a storage Addition JSON blob and returns only its
// credential fields, in a canonical map keyed by the ORIGINAL JSON key (so they
// can be overlaid back verbatim on a peer). Returns an empty map for an empty or
// unparseable blob.
func extractCreds(additionJSON string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if strings.TrimSpace(additionJSON) == "" {
		return out
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(additionJSON), &all); err != nil {
		return out
	}
	for k, v := range all {
		if isCredField(k) {
			out[k] = v
		}
	}
	return out
}

// credFieldNames returns the sorted credential field names present in a payload.
func credFieldNames(creds map[string]json.RawMessage) []string {
	names := make([]string, 0, len(creds))
	for k := range creds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// canonicalCreds serializes a credential map deterministically (keys sorted) so
// the same credentials always hash identically on every node.
func canonicalCreds(creds map[string]json.RawMessage) []byte {
	names := credFieldNames(creds)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(creds[k])
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// credHash is the idempotency key: identical credentials → identical hash on
// every node. An empty credential set hashes to "" so it is never propagated
// (the engine treats "" as "nothing to share / nothing learned").
func credHash(creds map[string]json.RawMessage) string {
	if len(creds) == 0 {
		return ""
	}
	sum := sha256.Sum256(canonicalCreds(creds))
	return hex.EncodeToString(sum[:])
}

// applyCreds overlays credential fields onto an existing Addition JSON blob and
// returns the updated blob. Only the keys present in creds are replaced; every
// other field of the target Addition is preserved exactly. Returns (json,true)
// when the blob actually changed, (original,false) when it was already identical
// (so the caller can skip a needless re-init / avoid churn).
func applyCreds(additionJSON string, creds map[string]json.RawMessage) (string, bool) {
	if len(creds) == 0 {
		return additionJSON, false
	}
	all := map[string]json.RawMessage{}
	if strings.TrimSpace(additionJSON) != "" {
		if err := json.Unmarshal([]byte(additionJSON), &all); err != nil {
			// Unparseable target: rebuild from creds alone rather than corrupt it.
			all = map[string]json.RawMessage{}
		}
	}
	changed := false
	for k, v := range creds {
		if cur, ok := all[k]; !ok || !jsonEqual(cur, v) {
			all[k] = v
			changed = true
		}
	}
	if !changed {
		return additionJSON, false
	}
	// Marshal with sorted keys for stability (encoding/json already sorts map keys).
	b, err := json.Marshal(all)
	if err != nil {
		return additionJSON, false
	}
	return string(b), true
}

// jsonEqual compares two raw JSON values semantically (after re-normalizing) so
// whitespace differences do not count as a change.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv interface{}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return string(a) == string(b)
	}
	an, _ := json.Marshal(av)
	bn, _ := json.Marshal(bv)
	return string(an) == string(bn)
}
