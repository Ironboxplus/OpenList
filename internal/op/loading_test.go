package op

import "testing"

func items() []LoadTarget {
	return []LoadTarget{
		{MountPath: "/a", Driver: "Local"},
		{MountPath: "/b", Driver: "OneDrive"},
		{MountPath: "/c", Driver: "Local"},
	}
}

func TestLoadingProgressBeginInitialisesPending(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())
	s := p.Snapshot()
	if s.Total != 3 {
		t.Fatalf("expected total 3, got %d", s.Total)
	}
	if s.Pending != 3 {
		t.Fatalf("expected 3 pending, got %d", s.Pending)
	}
	if s.Finished {
		t.Fatal("should not be finished right after Begin")
	}
	if len(s.Items) != 3 || s.Items[0].MountPath != "/a" {
		t.Fatalf("expected ordered items, got %+v", s.Items)
	}
	if s.Items[0].State != LoadPending {
		t.Fatalf("expected pending state, got %s", s.Items[0].State)
	}
}

func TestLoadingProgressTransitions(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())

	p.SetState("/a", LoadLoading, "")
	if got := p.Snapshot().Loading; got != 1 {
		t.Fatalf("expected 1 loading, got %d", got)
	}

	p.SetState("/a", LoadLoaded, "")
	p.SetState("/b", LoadLoaded, "")
	s := p.Snapshot()
	if s.Loaded != 2 {
		t.Fatalf("expected 2 loaded, got %d", s.Loaded)
	}
	if s.Finished {
		t.Fatal("should not be finished with one storage left")
	}
}

func TestLoadingProgressFailedRecordsError(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())
	p.SetState("/a", LoadFailed, "boom")
	var found *StorageLoadInfo
	for i := range p.Snapshot().Items {
		if p.Snapshot().Items[i].MountPath == "/a" {
			found = &p.Snapshot().Items[i]
		}
	}
	if found == nil || found.State != LoadFailed || found.Error != "boom" {
		t.Fatalf("expected failed with error, got %+v", found)
	}
	if p.Snapshot().Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", p.Snapshot().Failed)
	}
}

func TestLoadingProgressFinishedWhenAllTerminal(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())
	p.SetState("/a", LoadLoaded, "")
	p.SetState("/b", LoadFailed, "x")
	p.SetState("/c", LoadLoaded, "")
	s := p.Snapshot()
	if !s.Finished {
		t.Fatal("expected finished when all storages reached a terminal state")
	}
	if s.Loaded != 2 || s.Failed != 1 || s.Pending != 0 || s.Loading != 0 {
		t.Fatalf("unexpected counts: %+v", s)
	}
}

func TestLoadingProgressEmptyIsFinished(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(nil)
	s := p.Snapshot()
	if s.Total != 0 || !s.Finished {
		t.Fatalf("empty progress should be finished, got %+v", s)
	}
}

func TestLoadingProgressSetStateUnknownMountIsNoop(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())
	p.SetState("/does-not-exist", LoadLoaded, "")
	if p.Snapshot().Loaded != 0 {
		t.Fatal("setting state on an unknown mount must not change counts")
	}
}

func TestLoadingProgressSnapshotIsCopy(t *testing.T) {
	p := NewLoadingProgress()
	p.Begin(items())
	s := p.Snapshot()
	s.Items[0].State = LoadLoaded // mutate the copy
	if p.Snapshot().Items[0].State != LoadPending {
		t.Fatal("Snapshot must return a copy; internal state was mutated")
	}
}
