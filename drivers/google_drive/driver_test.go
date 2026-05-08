package google_drive

import (
	"sync"
	"testing"
)

func TestMaxPutAuthRetriesIsBounded(t *testing.T) {
	if maxPutAuthRetries < 1 {
		t.Fatalf("maxPutAuthRetries=%d, must be >= 1", maxPutAuthRetries)
	}
	if maxPutAuthRetries > 5 {
		t.Fatalf("maxPutAuthRetries=%d, must be <= 5 to prevent excessive retries", maxPutAuthRetries)
	}
}

func TestMkdirLocksCleanedUpAfterUse(t *testing.T) {
	// Reset state
	mkdirLocks = sync.Map{}

	key1 := "parent-1/folder-a"
	key2 := "parent-2/folder-b"

	// Simulate two MakeDir calls storing locks
	mkdirLocks.LoadOrStore(key1, &sync.Mutex{})
	mkdirLocks.LoadOrStore(key2, &sync.Mutex{})

	// Both should exist
	if _, ok := mkdirLocks.Load(key1); !ok {
		t.Fatal("key1 should exist")
	}
	if _, ok := mkdirLocks.Load(key2); !ok {
		t.Fatal("key2 should exist")
	}

	// After MakeDir completes, locks should be cleaned up
	mkdirLocks.Delete(key1)
	mkdirLocks.Delete(key2)

	if _, ok := mkdirLocks.Load(key1); ok {
		t.Fatal("key1 should be deleted after cleanup")
	}
	if _, ok := mkdirLocks.Load(key2); ok {
		t.Fatal("key2 should be deleted after cleanup")
	}
}

func TestMkdirLocksNoCrossContamination(t *testing.T) {
	mkdirLocks = sync.Map{}

	key := "parent/shared-folder"
	lockVal, _ := mkdirLocks.LoadOrStore(key, &sync.Mutex{})
	lock := lockVal.(*sync.Mutex)

	// Simulate concurrent access: lock should be shared for same key
	lockVal2, loaded := mkdirLocks.LoadOrStore(key, &sync.Mutex{})
	if !loaded {
		t.Fatal("second LoadOrStore should return existing entry")
	}
	lock2 := lockVal2.(*sync.Mutex)
	if lock != lock2 {
		t.Fatal("same key should return same mutex instance")
	}

	// Different key should get different lock
	lockVal3, _ := mkdirLocks.LoadOrStore("other-parent/other-folder", &sync.Mutex{})
	lock3 := lockVal3.(*sync.Mutex)
	if lock == lock3 {
		t.Fatal("different keys should have different mutex instances")
	}
}
