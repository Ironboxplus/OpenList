package cmd

import "testing"

func TestRecordNamesFileParses(t *testing.T) {
	names, err := readRecordNames("../internal/media/scraper/testdata/record_names.txt")
	if err != nil || len(names) < 20 {
		t.Fatalf("%d names, %v", len(names), err)
	}
}
