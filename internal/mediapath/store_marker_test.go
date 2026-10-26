package mediapath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreMarkerCheckAndWrite(t *testing.T) {
	root := t.TempDir()

	if err := CheckStoreMarker(root, "tokyo"); !errors.Is(err, ErrStoreMarkerMismatch) {
		t.Fatalf("CheckStoreMarker without marker = %v, want ErrStoreMarkerMismatch", err)
	}
	if err := WriteStoreMarker(root, "tokyo"); err != nil {
		t.Fatalf("WriteStoreMarker() error = %v", err)
	}
	if err := CheckStoreMarker(root, "tokyo"); err != nil {
		t.Fatalf("CheckStoreMarker() after write = %v", err)
	}
	if err := CheckStoreMarker(root, "osaka"); !errors.Is(err, ErrStoreMarkerMismatch) {
		t.Fatalf("CheckStoreMarker with wrong expected name = %v, want ErrStoreMarkerMismatch", err)
	}
	contents, err := os.ReadFile(filepath.Join(root, StoreMarkerFileName))
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if string(contents) != "tokyo" {
		t.Fatalf("marker contents = %q, want %q", contents, "tokyo")
	}
	if err := WriteStoreMarker(root, "tokyo"); err != nil {
		t.Fatalf("WriteStoreMarker() idempotent error = %v", err)
	}
	if err := WriteStoreMarker(root, "osaka"); !errors.Is(err, ErrStoreMarkerMismatch) {
		t.Fatalf("WriteStoreMarker() rename error = %v, want ErrStoreMarkerMismatch", err)
	}
	if err := CheckStoreMarker(root, "tokyo"); err != nil {
		t.Fatalf("CheckStoreMarker() after refused rename = %v", err)
	}
	contents, err = os.ReadFile(filepath.Join(root, StoreMarkerFileName))
	if err != nil {
		t.Fatalf("reading marker after refused rename: %v", err)
	}
	if string(contents) != "tokyo" {
		t.Fatalf("marker contents after refused rename = %q, want %q", contents, "tokyo")
	}
}

func TestCheckStoreMarkerSkipsEmptyExpectedName(t *testing.T) {
	if err := CheckStoreMarker(t.TempDir(), ""); err != nil {
		t.Fatalf("CheckStoreMarker(empty expected) = %v, want nil", err)
	}
}

func TestValidStoreName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "tokyo-2", want: true},
		{name: "", want: false},
		{name: "Tokyo", want: false},
		{name: "a_b", want: false},
		{name: "a/b", want: false},
		{name: "録画", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidStoreName(tc.name); got != tc.want {
				t.Errorf("ValidStoreName(%q) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}
