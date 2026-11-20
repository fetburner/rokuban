package dbcollector

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// 書き手の印が一度も無いとき、系列は 0 で出る（未成功と読み取り失敗を分けるため、0 は読み取れた証拠）。
func TestSyncMarkerCollector_ZeroBeforeAnyMarker(t *testing.T) {
	t.Parallel()
	fake := &fakeReconcileMarkers{}
	c := NewSyncMarkerCollector(fake, []string{"site-b", "site-a"})
	got := collectReconcileSeries(t, c)

	want := map[string]float64{
		"rokuban_epg_sync_last_success_timestamp_seconds":                  0,
		"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-a}":   0,
		"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-b}":   0,
		"rokuban_storage_transfer_last_success_timestamp_seconds":          0,
		"rokuban_sync_marker_scrape_errors_total{source=epg_sync}":         0,
		"rokuban_sync_marker_scrape_errors_total{source=tuner_sync}":       0,
		"rokuban_sync_marker_scrape_errors_total{source=storage_transfer}": 0,
	}
	assertExactSeries(t, got, want)
}

// EPG のラベルなし系列は設定された site の最大値。設定から外れた site の印は出さない。
// tuner は設定された site ごとに出し、印が無い site は 0。転送は置き場を問わない 1 行。
func TestSyncMarkerCollector_ReportsConfiguredSitesAndLatestEPG(t *testing.T) {
	t.Parallel()
	epgA := time.Unix(1700000100, 0)
	epgB := time.Unix(1700000300, 0) // 最新の設定 site
	epgZ := time.Unix(1700000900, 0) // 設定に無い site。出さない
	tunerA := time.Unix(1700000200, 0)
	tunerZ := time.Unix(1700000800, 0) // 設定に無い site。出さない
	transferAt := time.Unix(1700000500, 0)
	fake := &fakeReconcileMarkers{loops: map[string]map[string]time.Time{
		store.LoopEPGSync:         {"site-a": epgA, "site-b": epgB, "site-z": epgZ},
		store.LoopTunerSync:       {"site-a": tunerA, "site-z": tunerZ},
		store.LoopStorageTransfer: {store.StorageTransferScope: transferAt},
	}}
	c := NewSyncMarkerCollector(fake, []string{"site-a", "site-b"})
	got := collectReconcileSeries(t, c)

	want := map[string]float64{
		"rokuban_epg_sync_last_success_timestamp_seconds":                  float64(epgB.Unix()),
		"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-a}":   float64(tunerA.Unix()),
		"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-b}":   0,
		"rokuban_storage_transfer_last_success_timestamp_seconds":          float64(transferAt.Unix()),
		"rokuban_sync_marker_scrape_errors_total{source=epg_sync}":         0,
		"rokuban_sync_marker_scrape_errors_total{source=tuner_sync}":       0,
		"rokuban_sync_marker_scrape_errors_total{source=storage_transfer}": 0,
	}
	assertExactSeries(t, got, want)
}

// 1 つの読み取りが失敗したとき、その loop の系列だけが落ち（0 に置き換えず）、その source のカウンタだけが進む。
func TestSyncMarkerCollector_ReadFailureDropsOnlyItsSeries(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	at := time.Unix(1700000700, 0)
	cases := []struct {
		name   string
		source string
		failed []string
		kept   []string
	}{
		{
			name: "epg sync", source: store.LoopEPGSync,
			failed: []string{"rokuban_epg_sync_last_success_timestamp_seconds"},
			kept:   []string{"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-a}", "rokuban_storage_transfer_last_success_timestamp_seconds"},
		},
		{
			name: "tuner sync", source: store.LoopTunerSync,
			failed: []string{"rokuban_tuner_sync_last_success_timestamp_seconds{site=site-a}"},
			kept:   []string{"rokuban_epg_sync_last_success_timestamp_seconds", "rokuban_storage_transfer_last_success_timestamp_seconds"},
		},
		{
			name: "storage transfer", source: store.LoopStorageTransfer,
			failed: []string{"rokuban_storage_transfer_last_success_timestamp_seconds"},
			kept:   []string{"rokuban_epg_sync_last_success_timestamp_seconds", "rokuban_tuner_sync_last_success_timestamp_seconds{site=site-a}"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeReconcileMarkers{
				loops: map[string]map[string]time.Time{
					store.LoopEPGSync:         {"site-a": at},
					store.LoopTunerSync:       {"site-a": at},
					store.LoopStorageTransfer: {store.StorageTransferScope: at},
				},
				err: map[string]error{tc.source: errBoom},
			}
			c := NewSyncMarkerCollector(fake, []string{"site-a"})
			got := collectReconcileSeries(t, c)

			for _, key := range tc.failed {
				if _, ok := got[key]; ok {
					t.Errorf("series %q present after a failed %s read, want dropped (not zeroed)", key, tc.name)
				}
			}
			for _, key := range tc.kept {
				if v, ok := got[key]; !ok || v == 0 {
					t.Errorf("series %q = %v (present %t) after a failed %s read, want kept with its value", key, v, ok, tc.name)
				}
			}
			for _, source := range []string{store.LoopEPGSync, store.LoopTunerSync, store.LoopStorageTransfer} {
				key := "rokuban_sync_marker_scrape_errors_total{source=" + source + "}"
				want := 0.0
				if source == tc.source {
					want = 1
				}
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

// assertExactSeries は want と got の系列が完全に一致することを見る（エラーカウンタを含む）。
func assertExactSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("series %q missing", key)
			continue
		}
		if g != w {
			t.Errorf("series %q = %v, want %v", key, g, w)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok && !strings.Contains(key, "scrape_errors_total") {
			t.Errorf("unexpected series %q = %v", key, got[key])
		}
	}
}
