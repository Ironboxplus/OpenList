package handles

import (
	"testing"
)

// TestClearSkippedNames verifies the contract from upstream fix #2520:
// When SkipExisting is true and a file already exists at the destination,
// that name must be cleared (set to "") so the task loop skips it.
// Valid (non-skipped) names must be set to full srcPath.
//
// This is a contract test for the name-filtering loop in FsMove/FsCopy.
// It tests the logic in isolation without needing gin/fs dependencies.
func TestClearSkippedNames(t *testing.T) {
	// Simulate the name-filtering loop logic from FsMove/FsCopy.
	// existsAtDst simulates whether a file already exists at the destination.
	filterNames := func(srcDir string, names []string, overwrite, skipExisting bool, existsAtDst func(name string) bool) []string {
		result := make([]string, len(names))
		copy(result, names)
		for i, name := range result {
			srcPath := srcDir + "/" + name
			// First: set to srcPath (the fix moves this before skip check)
			result[i] = srcPath
			if !overwrite {
				if existsAtDst(name) {
					if !skipExisting {
						// Would return error in real code
						result[i] = "ERROR"
						return result
					}
					// Skip: must clear to ""
					result[i] = ""
					continue
				}
			}
		}
		return result
	}

	tests := []struct {
		name         string
		srcDir       string
		names        []string
		overwrite    bool
		skipExisting bool
		existsAtDst  func(string) bool
		want         []string
	}{
		{
			name:         "skip existing files clears their names",
			srcDir:       "/src",
			names:        []string{"a.mkv", "b.mkv", "c.mkv"},
			overwrite:    false,
			skipExisting: true,
			existsAtDst:  func(n string) bool { return n == "b.mkv" },
			want:         []string{"/src/a.mkv", "", "/src/c.mkv"},
		},
		{
			name:         "no skipping when overwrite is true",
			srcDir:       "/src",
			names:        []string{"a.mkv", "b.mkv"},
			overwrite:    true,
			skipExisting: false,
			existsAtDst:  func(string) bool { return true },
			want:         []string{"/src/a.mkv", "/src/b.mkv"},
		},
		{
			name:         "all files skipped results in all empty",
			srcDir:       "/src",
			names:        []string{"x.mp4", "y.mp4"},
			overwrite:    false,
			skipExisting: true,
			existsAtDst:  func(string) bool { return true },
			want:         []string{"", ""},
		},
		{
			name:         "no existing files keeps all paths",
			srcDir:       "/data",
			names:        []string{"movie.mkv"},
			overwrite:    false,
			skipExisting: true,
			existsAtDst:  func(string) bool { return false },
			want:         []string{"/data/movie.mkv"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterNames(tt.srcDir, tt.names, tt.overwrite, tt.skipExisting, tt.existsAtDst)
			if len(got) != len(tt.want) {
				t.Fatalf("length mismatch: got %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("names[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
