package config

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobs"
)

const validStoragePolicy = `
storage:
  media_dir: /mnt/media
  stores:
    main:
      kind: fs
      internal_url: http://store-main.internal:8080
    s3main:
      kind: s3
      endpoint: https://s3.example.com
      bucket: rokuban-derived
  originals:
    - match: { site: default, older_than: 30d }
      store: main
    - match: { site: default }
      store: main
    - store: main
  derived: main
  transfer:
    bandwidth: { main: 50Mbps }
    backlog_limit: { main: 20 }
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
	if cfg.Storage.Originals[0].Match == nil || cfg.Storage.Originals[0].Match.Site != "default" || cfg.Storage.Originals[0].Match.OlderThan != "30d" {
		t.Fatalf("originals[0].match = %+v, want site=default older_than=30d", cfg.Storage.Originals[0].Match)
	}
	if cfg.Storage.Stores["s3main"].Kind != "s3" || cfg.Storage.Derived != "main" {
		t.Fatalf("stores/derived = %+v/%q", cfg.Storage.Stores, cfg.Storage.Derived)
	}
	if got := cfg.Storage.TransferBacklogLimit("main"); got != 20 {
		t.Fatalf("TransferBacklogLimit(main) = %d, want 20", got)
	}
	if got := cfg.Storage.TransferBacklogLimit("s3main"); got != 0 {
		t.Fatalf("TransferBacklogLimit(s3main) = %d, want 0 (unset is unlimited)", got)
	}
}

// 2 つ以上の FS 置き場と、originals / derived が別の FS 置き場を指す構成は通る。
func TestLoadStoragePolicyAcceptsSeveralFSStores(t *testing.T) {
	policy := `
storage:
  stores:
    tokyo:     { kind: fs, root: /mnt/tokyo }
    takamatsu: { kind: fs, root: /mnt/takamatsu }
  originals:
    - match: { site: default }
      store: takamatsu
    - store: tokyo
  derived: tokyo
  scratch_dir: /tmp/rokuban
`
	cfg, err := loadStoragePolicy(t, policy)
	if err != nil {
		t.Fatalf("loading a two-fs-store configuration: %v", err)
	}
	if got := cfg.Storage.FSStoreNames(); len(got) != 2 || got[0] != "takamatsu" || got[1] != "tokyo" {
		t.Fatalf("FSStoreNames() = %v, want [takamatsu tokyo]", got)
	}
	// derived が originals と別の FS 置き場を指してもよい。
	other := strings.Replace(policy, "derived: tokyo", "derived: takamatsu", 1)
	if _, err := loadStoragePolicy(t, other); err != nil {
		t.Fatalf("derived pointing at a different fs store than the last original rule: %v", err)
	}
}

func TestStoreRoots(t *testing.T) {
	storage := StorageConfig{
		MediaDir: "/mnt/media",
		Stores: map[string]StoreConfig{
			"a":  {Kind: "fs", Root: "/mnt/a"},
			"b":  {Kind: "fs", Root: "/mnt/b"},
			"c":  {Kind: "fs"},
			"d":  {Kind: "fs"},
			"s3": {Kind: "s3", Bucket: "x"},
		},
	}
	got, err := storage.StoreRoots([]string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "/mnt/a" || got["b"] != "/mnt/b" || got["c"] != "/mnt/media" || len(got) != 3 {
		t.Fatalf("StoreRoots = %v, want a=/mnt/a b=/mnt/b c=/mnt/media", got)
	}
	for name, mounted := range map[string][]string{
		"two stores fall back to the same media_dir": {"c", "d"},
		"s3 cannot be mounted":                       {"s3"},
		"unknown store":                              {"missing"},
	} {
		if _, err := storage.StoreRoots(mounted); err == nil {
			t.Errorf("%s: StoreRoots(%v) succeeded, want error", name, mounted)
		}
	}
	if _, err := (StorageConfig{Stores: map[string]StoreConfig{"c": {Kind: "fs"}}}).StoreRoots([]string{"c"}); err == nil {
		t.Error("StoreRoots without root or media_dir succeeded, want error")
	}
}

// 置き場の root が入れ子・同一になる構成は拒み、パスの区切りで比べる（/mnt/media2 は /mnt/media の外）。
func TestStoreRootsRejectsNestedAndIdenticalRoots(t *testing.T) {
	stores := map[string]StoreConfig{
		"outer":   {Kind: "fs", Root: "/mnt/media"},
		"nested":  {Kind: "fs", Root: "/mnt/media/takamatsu"},
		"same":    {Kind: "fs", Root: "/mnt/media/"},
		"sibling": {Kind: "fs", Root: "/mnt/media2"},
		"other":   {Kind: "fs", Root: "/mnt/other"},
	}
	storage := StorageConfig{Stores: stores}
	for _, tt := range []struct {
		name    string
		mounted []string
		ok      bool
	}{
		{name: "nested root inside another is rejected", mounted: []string{"outer", "nested"}},
		{name: "nested root listed first is rejected too", mounted: []string{"nested", "outer"}},
		{name: "identical after clean is rejected", mounted: []string{"outer", "same"}},
		{name: "sibling sharing a string prefix is accepted", mounted: []string{"outer", "sibling"}, ok: true},
		{name: "unrelated roots are accepted", mounted: []string{"outer", "other"}, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := storage.StoreRoots(tt.mounted)
			if tt.ok && err != nil {
				t.Fatalf("StoreRoots(%v) error = %v, want nil", tt.mounted, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("StoreRoots(%v) error = nil, want error", tt.mounted)
			}
		})
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
			policy:    strings.Replace(validStoragePolicy, "    s3main:\n", "    S3main:\n", 1),
			wantError: "storage.stores key \"S3main\"",
		},
		{
			name:      "uppercase fs store name",
			policy:    strings.Replace(validStoragePolicy, "    main:\n", "    Main:\n", 1),
			wantError: "storage.stores key \"Main\"",
		},
		{
			name:      "store name longer than queue budget",
			policy:    strings.Replace(validStoragePolicy, "    main:\n", "    "+strings.Repeat("a", MaxStoreNameLen+1)+":\n", 1),
			wantError: "exceeds 54 characters",
		},
		{
			name:      "unsupported store kind",
			policy:    strings.Replace(validStoragePolicy, "kind: s3", "kind: nfs", 1),
			wantError: "kind must be fs or s3",
		},
		{
			name:      "s3 store requires bucket",
			policy:    strings.Replace(validStoragePolicy, "      bucket: rokuban-derived\n", "", 1),
			wantError: "bucket is required",
		},
		{
			name:      "fs store rejects s3 attributes",
			policy:    strings.Replace(validStoragePolicy, "      internal_url: http://store-main.internal:8080\n", "      endpoint: https://s3.example.com\n", 1),
			wantError: "endpoint, region, and bucket are only valid for kind s3",
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
			policy:    strings.Replace(validStoragePolicy, "      store: main\n    - match: { site: default }", "      store: s3main\n    - match: { site: default }", 1),
			wantError: "must reference an fs store",
		},
		{
			name:      "store must exist",
			policy:    strings.Replace(validStoragePolicy, "      store: main\n    - match: { site: default }", "      store: missing\n    - match: { site: default }", 1),
			wantError: "references unknown store",
		},
		{
			name:      "backlog_limit references unknown store",
			policy:    strings.Replace(validStoragePolicy, "backlog_limit: { main: 20 }", "backlog_limit: { nope: 20 }", 1),
			wantError: "storage.transfer.backlog_limit references unknown store",
		},
		{
			name:      "backlog_limit store must be fs",
			policy:    strings.Replace(validStoragePolicy, "backlog_limit: { main: 20 }", "backlog_limit: { s3main: 20 }", 1),
			wantError: "storage.transfer.backlog_limit.s3main: store must be kind fs",
		},
		{
			name:      "backlog_limit must be positive",
			policy:    strings.Replace(validStoragePolicy, "backlog_limit: { main: 20 }", "backlog_limit: { main: 0 }", 1),
			wantError: "storage.transfer.backlog_limit.main must be a positive integer",
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
			name:      "no fs store",
			policy:    strings.ReplaceAll(strings.Replace(validStoragePolicy, "    main:\n      kind: fs\n      internal_url: http://store-main.internal:8080\n", "", 1), "store: main", "store: s3main"),
			wantError: "must reference an fs store",
		},
		{
			name:      "root is only for fs stores",
			policy:    strings.Replace(validStoragePolicy, "      bucket: rokuban-derived", "      bucket: rokuban-derived\n      root: /mnt/x", 1),
			wantError: "root is only valid for kind fs",
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

func TestStorageDerivedMayUseS3Store(t *testing.T) {
	policy := strings.Replace(validStoragePolicy, "derived: main", "derived: s3main", 1)
	policy = strings.Replace(policy, "      endpoint: https://s3.example.com", "      endpoint: https://s3.example.com\n      region: auto", 1)
	cfg, err := loadStoragePolicy(t, policy)
	if err != nil {
		t.Fatalf("loading S3 derived store configuration: %v", err)
	}
	if cfg.Storage.Derived != "s3main" || cfg.Storage.Stores[cfg.Storage.Derived].Kind != "s3" {
		t.Fatalf("derived store = %q (%+v), want s3main with kind s3", cfg.Storage.Derived, cfg.Storage.Stores[cfg.Storage.Derived])
	}
	if cfg.Storage.Stores[cfg.Storage.Derived].Region != "auto" {
		t.Fatalf("S3 region = %q, want auto", cfg.Storage.Stores[cfg.Storage.Derived].Region)
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
	want := `[{"match":{"site":"default","older_than_us":2592000000000},"store":"archive"},{"store":"main"}]`
	if string(got) != want {
		t.Fatalf("rules JSON = %s, want %s", got, want)
	}
}

func TestStorageTransferBandwidthBytesPerSecond(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
		bad   bool
	}{
		{value: "50Mbps", want: 6_250_000},
		{value: "1.5MiBps", want: 1_572_864},
		{value: "10MBps", want: 10_000_000},
		{value: "8bps", want: 1},
		{value: "1", bad: true},
		{value: "0Mbps", bad: true},
		{value: "NaNMbps", bad: true},
		{value: "1tbps", bad: true},
	} {
		got, err := (StorageConfig{Transfer: StorageTransferConfig{Bandwidth: map[string]string{"source": tc.value}}}).TransferBandwidthBytesPerSecond("source")
		if tc.bad {
			if err == nil {
				t.Errorf("bandwidth %q error = nil, want error", tc.value)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("bandwidth %q = %d, %v; want %d, nil", tc.value, got, err, tc.want)
		}
	}
	if got, err := (StorageConfig{}).TransferBandwidthBytesPerSecond("source"); err != nil || got != 0 {
		t.Errorf("unset bandwidth = %d, %v; want 0, nil", got, err)
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
			MediaDir:      "/mnt/media",
			ScratchDir:    "/tmp/rokuban",
			AccelLocation: "/_media/",
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

	changedTransfer := newStorage()
	changedTransfer.Transfer.Bandwidth = map[string]string{"main": "10Mbps"}
	changedTransfer.Transfer.Overlap = 72 * time.Hour
	changedTransfer.Transfer.BacklogLimit = map[string]int{"main": 5}
	got, err := changedTransfer.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint changed after transfer.bandwidth/backlog_limit/overlap changes: got %s want %s", got, want)
	}

	changedNonPolicy := newStorage()
	changedNonPolicy.MediaDir = "/mnt/other"
	changedNonPolicy.ScratchDir = "/tmp/other"
	changedNonPolicy.AccelLocation = "/private-media/"
	changedNonPolicy.Stores["main"] = StoreConfig{Kind: "fs", InternalURL: "http://main.internal:8080", Root: "/mnt/elsewhere"}
	got, err = changedNonPolicy.PolicyFingerprint()
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

// TestStorageQueueNameBudget は、置き場で修飾されうるキューの最長名と置き場の名前の上限が、
// River のキュー名の上限 64 文字に収まることを固定する。
// 原本を読み書きしない 5 本（default / ruler / epg / reconciler / watcher）以外は、
// 新しいキューも置き場で修飾されうるものとして数える。
func TestStorageQueueNameBudget(t *testing.T) {
	notStoreBound := map[string]bool{"default": true, "ruler": true, "epg": true, "reconciler": true, "watcher": true}
	longest := ""
	for _, queue := range jobs.AllQueueNames() {
		if notStoreBound[queue] {
			continue
		}
		if len(queue) > len(longest) {
			longest = queue
		}
		if got := len(queue) + 1 + MaxStoreNameLen; got > 64 {
			t.Errorf("queue %q qualified by a %d-character store name is %d characters, want <= 64", queue, MaxStoreNameLen, got)
		}
	}
	if len(longest) != 9 {
		t.Fatalf("longest store-bound queue %q has %d characters, want 9 (update MaxStoreNameLen with it)", longest, len(longest))
	}
	if MaxStoreNameLen != 54 {
		t.Fatalf("MaxStoreNameLen = %d, want 54", MaxStoreNameLen)
	}
}

func TestStoragePolicyOverlapDefault(t *testing.T) {
	policy := strings.Replace(validStoragePolicy, "  transfer:\n    bandwidth: { main: 50Mbps }\n    backlog_limit: { main: 20 }\n    overlap: 48h\n", "", 1)
	cfg, err := loadStoragePolicy(t, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Storage.Transfer.Overlap; got != 48*time.Hour {
		t.Fatalf("default overlap = %s, want 48h", got)
	}

	// 長い実体無し猶予では既定の overlap が下限を割るので、明示を求める。
	_, err = loadStoragePolicy(t, policy+"cleanup:\n  missing_asset_age: 48h\n")
	if err == nil || !strings.Contains(err.Error(), "set it explicitly") {
		t.Fatalf("error = %v, want a request to set overlap explicitly", err)
	}
	explicit := strings.Replace(validStoragePolicy, "overlap: 48h", "overlap: 72h", 1) + "cleanup:\n  missing_asset_age: 48h\n"
	if _, err := loadStoragePolicy(t, explicit); err != nil {
		t.Fatalf("explicit overlap: %v", err)
	}
}

// TestLoadRejectsInternalURLHostOutsideAllowedHosts は、server.allowed_hosts が空でないとき、
// internal_url のホストが一覧に無い構成を起動時に拒むことを固定する。拒まないと、転送の worker が
// streamer の Host 検証で 400 を受け、転送が必ず失敗する（kind の 2 置き場 e2e 判定 6.3 で実測）。
func TestLoadRejectsInternalURLHostOutsideAllowedHosts(t *testing.T) {
	cases := []struct {
		name        string
		server      string
		internalURL string
		wantError   string
	}{
		{"host not listed", "server:\n  allowed_hosts: [rokuban.local]\n", "", `storage.stores.main.internal_url host "store-main.internal" is not in server.allowed_hosts`},
		{"allowed host with different case and trailing dot", "server:\n  allowed_hosts: [rokuban.local, STORE-MAIN.internal.]\n", "", ""},
		{"URL host with different case, trailing dot and a port", "server:\n  allowed_hosts: [rokuban.local, store-main.internal]\n", "http://Store-Main.Internal.:8080", ""},
		{"empty allowed_hosts disables the check", "server:\n  allowed_hosts: []\n", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validStoragePolicy
			if tc.internalURL != "" {
				policy = strings.Replace(policy, "internal_url: http://store-main.internal:8080", "internal_url: "+tc.internalURL, 1)
			}
			_, err := loadStoragePolicy(t, tc.server+policy)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("Load = %v, want success", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.wantError)
			}
		})
	}
}
