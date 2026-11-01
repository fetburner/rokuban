package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/epgimport"
)

func TestPrintLibraryImportResultReportsTombstoneRenameGuidance(t *testing.T) {
	var out bytes.Buffer
	printLibraryImportResult(&out, epgimport.LibraryImportResult{
		Registered:             3,
		Skipped:                1,
		SkippedConflicts:       2,
		SkippedTombstonedPaths: 1,
	})
	got := out.String()
	if !strings.Contains(got, "conflicts:  2") {
		t.Errorf("output = %q, want total conflict count", got)
	}
	if !strings.Contains(got, "tombstones: 1 (move or rename the file to register it again)") {
		t.Errorf("output = %q, want tombstone count and rename guidance", got)
	}
}
