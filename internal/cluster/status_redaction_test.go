package cluster

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

const leakyStatus = `failed init: GET https://api.example.com/x?access_token=SECRET: 401 {"refresh_token":"SECRET"}`

func leakyNode() *nodeInfo {
	return &nodeInfo{
		NodeID:  "N1",
		Addr:    "https://n1",
		Version: 1,
		Storages: []storageInfo{
			{MountPath: "/ok", Driver: "Local", Status: op.WORK},
			{MountPath: "/bad", Driver: "Baidu", Status: leakyStatus},
		},
	}
}

func TestPublicStatus(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"work", op.WORK, op.WORK},
		{"disabled", op.DISABLED, op.DISABLED},
		{"empty", "", storageStatusError},
		{"provider error text", leakyStatus, storageStatusError},
	}
	for _, tc := range tests {
		if got := publicStatus(tc.in); got != tc.want {
			t.Errorf("%s: publicStatus(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestStorageInfoOfHidesRawStatus(t *testing.T) {
	got := storageInfoOf(model.Storage{MountPath: "/bad", Driver: "Baidu", Status: leakyStatus})
	want := storageInfo{MountPath: "/bad", Driver: "Baidu", Status: storageStatusError}
	if got != want {
		t.Fatalf("storageInfoOf = %#v, want %#v", got, want)
	}
}

func TestMergeInventoryScrubsStatusWithoutMutatingCaller(t *testing.T) {
	s := newStore()
	in := leakyNode()
	s.mergeInventory(in, 1000)
	list := s.inventoryList()
	if len(list) != 1 || list[0].Storages[0].Status != op.WORK || list[0].Storages[1].Status != storageStatusError {
		t.Fatalf("inventory statuses not scrubbed: %#v", list)
	}
	if in.Storages[1].Status != leakyStatus {
		t.Fatalf("caller's node info was modified: %#v", in.Storages)
	}
}

func TestSetLocalInventoryScrubsStatus(t *testing.T) {
	s := newStore()
	s.setLocalInventory(leakyNode())
	if got := s.inventoryList()[0].Storages[1].Status; got != storageStatusError {
		t.Fatalf("local inventory status = %q", got)
	}
}

func TestLoadScrubsLegacyPersistedStatus(t *testing.T) {
	old := newStore()
	old.mu.Lock()
	old.inventory["N1"] = leakyNode() // what an old version would have written
	old.mu.Unlock()
	raw, err := json.Marshal(old.export())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("SECRET")) {
		t.Fatal("fixture should contain the legacy secret")
	}

	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		t.Fatal(err)
	}
	s := newStore()
	s.load(ps)
	if got := s.inventoryList()[0].Storages[1].Status; got != storageStatusError {
		t.Fatalf("restored status = %q", got)
	}
	out, err := json.Marshal(s.export())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("SECRET")) {
		t.Fatalf("re-persisted state still holds the secret: %s", out)
	}
}

func TestStatusJSONHasNoRawStorageStatus(t *testing.T) {
	s := newStore()
	s.mergeInventory(leakyNode(), now())
	m := &Manager{id: &identity{NodeID: "SELF"}, state: s, conns: newConnRegistry(), cfgStore: &configStore{}}
	out, err := json.Marshal(m.Status())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("SECRET")) {
		t.Fatalf("Status() leaks raw storage status: %s", out)
	}
}
