package config

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
)

const validStoragePolicy = `
storage:
  media_dir: /mnt/media
  stores:
    main:
      kind: fs
      internal_url: http://store-main.internal:8080
    archive:
      kind: fs
    s3main:
      kind: s3
      endpoint: https://s3.example.com
      bucket: rokuban-derived
  originals:
    - match: { site: default, older_than: 30d }
      store: archive
    - match: { site: default }
      store: main
    - store: main
  derived: s3main
  transfer:
    bandwidth: { main: 50Mbps }
    overlap: 48h
`

func loadStoragePolicy(t *testing.T, policy string) (*Config, error) {
	t.Helper()
	base := strings.Replace(minimalConfig, "storage:\n  media_dir: /mnt/media\n", "", 1)
	return Load(writeConfig(t, base+policy))
}

func TestLoadStoragePolicy(t *testing.T) {
	cfg, err := loadStoragePolicy(t, validStoragePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Storage.MediaStoreName; got != "" {
		t.Fatalf("multi-fs compatibility name = %q, want empty", got)
	}
	if cfg.Storage.Originals[0].Match == nil || cfg.Storage.Originals[0].Match.Site != "default" || cfg.Storage.Originals[0].Match.OlderThan != "30d" {
		t.Fatalf("originals[0].match = %+v, want site=default older_than=30d", cfg.Storage.Originals[0].Match)
	}
	if cfg.Storage.Stores["s3main"].Kind != "s3" || cfg.Storage.Derived != "s3main" {
		t.Fatalf("stores/derived = %+v/%q", cfg.Storage.Stores, cfg.Storage.Derived)
	}
}

func TestSingleFSStoreCompatibilityName(t *testing.T) {
	policy := `
storage:
  media_dir: /mnt/media
  stores:
    main:
      kind: fs
  originals:
    - store: main
  derived: main
  transfer:
    overlap: 48h
`
	cfg, err := loadStoragePolicy(t, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Storage.MediaStoreName; got != "main" {
		t.Fatalf("single fs compatibility name = %q, want main", got)
	}
}

func TestLoadRejectsRemovedMediaStoreNameKey(t *testing.T) {
	legacy := strings.Replace(minimalConfig, "media_dir: /mnt/media", "media_dir: /mnt/media\n  media_store_name: main", 1)
	_, err := Load(writeConfig(t, legacy))
	if err == nil || !strings.Contains(err.Error(), "media_store_name") {
		t.Fatalf("error = %v, want strict parsing to reject the removed key", err)
	}
}

func TestStoragePolicyValidation(t *testing.T) {
	tests := []struct {
		name      string
		policy    string
		wantError string
	}{
		{
			name:      "uppercase store name",
			policy:    strings.Replace(validStoragePolicy, "    main:\n", "    Main:\n", 1),
			wantError: "storage.stores key \"Main\"",
		},
		{
			name:      "store name longer than queue budget",
			policy:    strings.Replace(validStoragePolicy, "    main:\n", "    "+strings.Repeat("a", MaxStoreNameLen+1)+":\n", 1),
			wantError: "exceeds 54 characters",
		},
		{
			name:      "unsupported time condition",
			policy:    strings.Replace(validStoragePolicy, "older_than: 30d", "newer_than: 30d", 1),
			wantError: "newer_than",
		},
		{
			name:      "zero older than",
			policy:    strings.Replace(validStoragePolicy, "older_than: 30d", "older_than: 0s", 1),
			wantError: "must be a positive duration",
		},
		{
			name:      "older than below database precision",
			policy:    strings.Replace(validStoragePolicy, "older_than: 30d", "older_than: 1ns", 1),
			wantError: "microsecond precision",
		},
		{
			name:      "last rule is conditional",
			policy:    strings.Replace(validStoragePolicy, "    - store: main\n", "    - match: { site: default }\n      store: main\n", 1),
			wantError: "must be unconditional",
		},
		{
			name:      "only final rule may be unconditional",
			policy:    strings.Replace(validStoragePolicy, "    - match: { site: default }\n      store: main\n", "    - store: main\n    - match: { site: default }\n      store: main\n", 1),
			wantError: "only the final rule may be unconditional",
		},
		{
			name:      "original store must be fs",
			policy:    strings.Replace(validStoragePolicy, "      store: archive\n", "      store: s3main\n", 1),
			wantError: "must reference an fs store",
		},
		{
			name:      "store must exist",
			policy:    strings.Replace(validStoragePolicy, "      store: archive\n", "      store: missing\n", 1),
			wantError: "references unknown store",
		},
		{
			name:      "overlap equals default lower bound",
			policy:    strings.Replace(validStoragePolicy, "overlap: 48h", "overlap: 24h30m", 1),
			wantError: "overlap must be greater than",
		},
		{
			name:      "overlap honors configured missing age",
			policy:    strings.Replace(validStoragePolicy, "overlap: 48h", "overlap: 12h30m", 1) + "cleanup:\n  missing_asset_age: 12h\n",
			wantError: "overlap must be greater than",
		},
		{
			name:      "invalid internal url scheme",
			policy:    strings.Replace(validStoragePolicy, "http://store-main.internal:8080", "ftp://store-main.internal:8080", 1),
			wantError: "internal_url",
		},
		{
			name:      "relative internal url",
			policy:    strings.Replace(validStoragePolicy, "http://store-main.internal:8080", "/store", 1),
			wantError: "internal_url",
		},
		{
			name:      "internal url query is rejected",
			policy:    strings.Replace(validStoragePolicy, "http://store-main.internal:8080", "http://store-main.internal:8080/?token=x", 1),
			wantError: "internal_url",
		},
		{
			name:      "invalid s3 endpoint",
			policy:    strings.Replace(validStoragePolicy, "https://s3.example.com", "s3.example.com", 1),
			wantError: "endpoint",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadStoragePolicy(t, tc.policy)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantError)
			}
		})
	}
}

func TestStoragePolicyOverlapIsStrictlyGreaterThanMissingAgePlusTwoDeletePasses(t *testing.T) {
	for _, tc := range []struct {
		overlap  string
		wantPass bool
	}{
		{overlap: "24h30m", wantPass: false},
		{overlap: "24h30m1ns", wantPass: true},
	} {
		policy := strings.Replace(validStoragePolicy, "overlap: 48h", "overlap: "+tc.overlap, 1)
		_, err := loadStoragePolicy(t, policy)
		if gotPass := err == nil; gotPass != tc.wantPass {
			t.Errorf("overlap %s passes = %t, want %t (error %v)", tc.overlap, gotPass, tc.wantPass, err)
		}
	}
}

func TestStoragePolicyOlderThanJSONNormalizesDuration(t *testing.T) {
	storage := StorageConfig{Originals: []OriginalStoreRule{
		{Match: &OriginalStoreMatch{Site: "default", OlderThan: "30d"}, Store: "archive"},
		{Store: "main"},
	}}
	got, err := storage.OriginalRulesJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"match":{"site":"default","older_than_ns":2592000000000000},"store":"archive"},{"store":"main"}]`
	if string(got) != want {
		t.Fatalf("rules JSON = %s, want %s", got, want)
	}
}

func TestStoragePolicyFingerprint(t *testing.T) {
	newStorage := func() StorageConfig {
		return StorageConfig{
			Stores: map[string]StoreConfig{
				"main":   {Kind: "fs", InternalURL: "http://main.internal:8080"},
				"s3main": {Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "derived"},
			},
			Originals: []OriginalStoreRule{
				{Match: &OriginalStoreMatch{Site: "default", OlderThan: "30d"}, Store: "main"},
				{Store: "main"},
			},
			Derived: "s3main",
			Transfer: StorageTransferConfig{
				Bandwidth: map[string]string{"main": "50Mbps"},
				Overlap:   48 * time.Hour,
			},
			MediaDir:       "/mnt/media",
			ScratchDir:     "/tmp/rokuban",
			AccelLocation:  "/_media/",
			MediaStoreName: "main",
		}
	}
	base := newStorage()
	want, err := base.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 64 {
		t.Fatalf("sha256 hex length = %d, want 64", len(want))
	}

	changedDefinition := newStorage()
	changedDefinition.Stores["main"] = StoreConfig{Kind: "fs", InternalURL: "http://other.internal:8080"}
	changedRule := newStorage()
	changedRule.Originals[0].Match.OlderThan = "31d"
	changedDerived := newStorage()
	changedDerived.Derived = "main"
	for name, changed := range map[string]StorageConfig{
		"store definition": changedDefinition,
		"original rule":    changedRule,
		"derived store":    changedDerived,
	} {
		got, err := changed.PolicyFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("fingerprint unchanged after changing %s", name)
		}
	}

	changedNonPolicy := newStorage()
	changedNonPolicy.MediaDir = "/mnt/other"
	changedNonPolicy.ScratchDir = "/tmp/other"
	changedNonPolicy.AccelLocation = "/private-media/"
	changedNonPolicy.MediaStoreName = "other"
	got, err := changedNonPolicy.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint changed after media path changes: got %s want %s", got, want)
	}

	mapOrder := StorageConfig{
		Stores: map[string]StoreConfig{
			"s3main": {Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "derived"},
			"main":   {Kind: "fs", InternalURL: "http://main.internal:8080"},
		},
		Originals: base.Originals,
		Derived:   base.Derived,
		Transfer:  base.Transfer,
	}
	got, err = mapOrder.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint depends on map insertion order: got %s want %s", got, want)
	}

	equivalentDuration := newStorage()
	equivalentDuration.Originals[0].Match.OlderThan = "720h"
	got, err = equivalentDuration.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint depends on equivalent duration spelling: got %s want %s", got, want)
	}
}

func TestStorageQueueNameBudget(t *testing.T) {
	if got := len("cm_detect") + 1 + MaxStoreNameLen; got != jobq.MaxQueueNameLen {
		t.Fatalf("queue length = %d, want jobq limit %d", got, jobq.MaxQueueNameLen)
	}
}
