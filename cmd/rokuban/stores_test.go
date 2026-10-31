package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
)

func newStoresTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringSlice(storeFlagName, nil, "")
	return cmd
}

// storageWithFSStores は FS 置き場 tokyo / takamatsu と S3 置き場 s3main を持つ storage を返す。
// 単一 FS の互換名（MediaStoreName）は設定しない（今の検証は 2 つ以上の FS を弾くので、
// resolveStoreBinding の検査だけを見るときは config.Load を通さない）。
func storageWithFSStores(fs ...string) config.StorageConfig {
	stores := map[string]config.StoreConfig{"s3main": {Kind: "s3", Bucket: "rokuban-derived"}}
	for _, name := range fs {
		stores[name] = config.StoreConfig{Kind: "fs"}
	}
	return config.StorageConfig{Stores: stores}
}

func TestResolveStoreBinding_UnspecifiedUsesSoleFSStore(t *testing.T) {
	got, err := resolveStoreBinding(newStoresTestCmd(t), storageWithFSStores("tokyo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(got, []string{"tokyo"}) {
		t.Errorf("mounted = %v, want [tokyo]", got)
	}
}

func TestResolveStoreBinding_UnspecifiedMultipleFSStores_IsError(t *testing.T) {
	_, err := resolveStoreBinding(newStoresTestCmd(t), storageWithFSStores("tokyo", "takamatsu"))
	if err == nil {
		t.Fatal("expected error when --stores is omitted with two fs stores, got nil")
	}
}

func TestResolveStoreBinding_UnspecifiedNoFSStore_MountsNone(t *testing.T) {
	got, err := resolveStoreBinding(newStoresTestCmd(t), storageWithFSStores())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("mounted = %v, want none", got)
	}
}

// --stores=（明示的な空）は置き場を持たない。省略（既定で 1 つマウント）とは区別される。
func TestResolveStoreBinding_ExplicitEmpty_MountsNone(t *testing.T) {
	cmd := newStoresTestCmd(t)
	if err := cmd.Flags().Set(storeFlagName, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := resolveStoreBinding(cmd, storageWithFSStores("tokyo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("mounted = %v, want none", got)
	}
}

func TestResolveStoreBinding_ExplicitNamesAreDeduplicated(t *testing.T) {
	cmd := newStoresTestCmd(t)
	if err := cmd.Flags().Set(storeFlagName, "tokyo,tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := resolveStoreBinding(cmd, storageWithFSStores("tokyo", "takamatsu"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(got, []string{"tokyo"}) {
		t.Errorf("mounted = %v, want [tokyo]", got)
	}
}

func TestResolveStoreBinding_RejectsUnknownAndNonFSStores(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unknown name", value: "nagoya", want: "unknown store(s) nagoya"},
		{name: "s3 store is not mountable", value: "s3main", want: "s3main is not an fs store"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newStoresTestCmd(t)
			if err := cmd.Flags().Set(storeFlagName, tt.value); err != nil {
				t.Fatalf("Set: %v", err)
			}
			_, err := resolveStoreBinding(cmd, storageWithFSStores("tokyo"))
			if err == nil {
				t.Fatalf("resolveStoreBinding(--stores %s) error = nil, want %q", tt.value, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// validateStoreBinding が落とす組み合わせを、ロール・キュー・--once の軸ごとに固定する。
// 受理側（mounted が足りる・キューが置き場を引かない）も併せて見る。
func TestValidateStoreBinding(t *testing.T) {
	one := []string{"tokyo"}
	two := []string{"tokyo", "takamatsu"}
	tests := []struct {
		name     string
		roles    []string
		explicit bool
		mounted  []string
		queues   []string
		once     bool
		wantErr  bool
	}{
		// 効かない指定
		{name: "explicit stores on api only is rejected", roles: []string{"api"}, explicit: true, mounted: one, wantErr: true},
		{name: "explicit stores on watcher only is rejected", roles: []string{"watcher"}, explicit: true, mounted: one, wantErr: true},
		{name: "implicit stores on api is accepted", roles: []string{"api"}, mounted: one},

		// --stores= で置き場で修飾するキューを引く
		{name: "empty stores with transfer queue is rejected", roles: []string{"worker"}, explicit: true, queues: []string{"transfer"}, wantErr: true},
		{name: "empty stores with all queues is rejected", roles: []string{"worker"}, explicit: true, wantErr: true},
		{name: "empty stores with ruler only is accepted", roles: []string{"worker"}, explicit: true, queues: []string{"ruler"}},

		// ingest は FS 置き場へ書く
		{name: "empty stores with ingest is rejected", roles: []string{"worker"}, explicit: true, queues: []string{"ingest"}, wantErr: true},
		{name: "empty stores with ingest and encode is rejected", roles: []string{"worker"}, explicit: true, queues: []string{"ingest", "encode"}, wantErr: true},
		{name: "ingest with one fs store is accepted", roles: []string{"worker"}, mounted: one, queues: []string{"ingest"}},

		// --once は物理キュー 1 本
		{name: "once with transfer on two stores is rejected", roles: []string{"worker"}, explicit: true, mounted: two, queues: []string{"transfer"}, once: true, wantErr: true},
		{name: "once with transfer on one store is accepted", roles: []string{"worker"}, explicit: true, mounted: one, queues: []string{"transfer"}, once: true},
		{name: "once with ruler on two stores is accepted", roles: []string{"worker"}, explicit: true, mounted: two, queues: []string{"ruler"}, once: true},

		// streamer は原本を FS から配信する
		{name: "streamer without a mounted store is rejected", roles: []string{"streamer"}, explicit: true, wantErr: true},
		{name: "streamer with a mounted store is accepted", roles: []string{"streamer"}, explicit: true, mounted: one},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStoreBinding(tt.roles, tt.explicit, tt.mounted, tt.queues, tt.once)
			if tt.wantErr && err == nil {
				t.Fatal("validateStoreBinding() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateStoreBinding() error = %v, want nil", err)
			}
		})
	}
}
