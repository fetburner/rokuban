package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/fetburner/rokuban/internal/store"
)

// fakeReconcileMarkers は loop_success_markers の読み取りと置き場の観測の読み取りを差し替える。
// loops は loop 名ごとの scope → 時刻。err のキーは loop 名（reconcile_pass / delete_reconcile は
// source 名と同じ値）か reconcileMarkerSourceStorage で、そこへの読み取りを失敗させる。
type fakeReconcileMarkers struct {
	loops   map[string]map[string]time.Time
	storage []store.StorageSync
	err     map[string]error
}

func (f *fakeReconcileMarkers) MarkRulerPasses(context.Context, []string) error { return nil }

func (f *fakeReconcileMarkers) MarkRecordSweep(context.Context, string) error { return nil }

func (f *fakeReconcileMarkers) MarkReconcilePass(context.Context, string) error { return nil }

func (f *fakeReconcileMarkers) MarkDeleteReconcile(context.Context, string) error { return nil }

func (f *fakeReconcileMarkers) MarkReconciler(context.Context, string) error { return nil }

func (f *fakeReconcileMarkers) ReadLoopMarkers(_ context.Context, loop string) (map[string]time.Time, error) {
	return f.loops[loop], f.err[loop]
}

func (f *fakeReconcileMarkers) ListStorageSync(context.Context) ([]store.StorageSync, error) {
	return f.storage, f.err[reconcileMarkerSourceStorage]
}

// collectReconcileSeries は Collect が出した系列を "name{label=value,...}" の形の
// キーで引けるようにし、gauge と counter の値を返す。
func collectReconcileSeries(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)
	out := make(map[string]float64)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		name := descName(t, m.Desc().String())
		labels := make([]string, 0, len(pb.Label))
		for _, l := range pb.Label {
			labels = append(labels, l.GetName()+"="+l.GetValue())
		}
		sort.Strings(labels)
		key := name
		if len(labels) > 0 {
			key += "{" + strings.Join(labels, ",") + "}"
		}
		switch {
		case pb.Gauge != nil:
			out[key] = pb.Gauge.GetValue()
		case pb.Counter != nil:
			out[key] = pb.Counter.GetValue()
		default:
			t.Fatalf("metric %q is neither gauge nor counter", key)
		}
	}
	return out
}

// descName は prometheus.Desc.String() の fqName を取り出す。
func descName(t *testing.T, desc string) string {
	t.Helper()
	const marker = `fqName: "`
	i := strings.Index(desc, marker)
	if i < 0 {
		t.Fatalf("desc %q has no fqName", desc)
	}
	rest := desc[i+len(marker):]
	j := strings.Index(rest, `"`)
	return rest[:j]
}

func TestReconcileMarkerCollector_ZeroBeforeAnyPass(t *testing.T) {
	t.Parallel()
	fake := &fakeReconcileMarkers{}
	c := NewReconcileMarkerCollector(fake, fake, []string{"fs-b", "fs-a"}, []string{"fs-b", "fs-a"})
	got := collectReconcileSeries(t, c)

	want := map[string]float64{
		"rokuban_encode_reconcile_last_success_timestamp_seconds":                0,
		"rokuban_thumbnail_reconcile_last_success_timestamp_seconds":             0,
		"rokuban_ts_scan_reconcile_last_success_timestamp_seconds":               0,
		"rokuban_delete_reconcile_last_success_timestamp_seconds{store=fs-a}":    0,
		"rokuban_delete_reconcile_last_success_timestamp_seconds{store=fs-b}":    0,
		"rokuban_storage_root_observed_timestamp_seconds{root=media,store=fs-a}": 0,
		"rokuban_storage_root_observed_timestamp_seconds{root=media,store=fs-b}": 0,
		"rokuban_reconcile_marker_scrape_errors_total{source=delete_reconcile}":  0,
		"rokuban_reconcile_marker_scrape_errors_total{source=reconcile_pass}":    0,
		"rokuban_reconcile_marker_scrape_errors_total{source=storage_sync}":      0,
	}
	assertSeries(t, got, want)
}

func TestReconcileMarkerCollector_ReportsSavedTimesOfConfiguredStoresOnly(t *testing.T) {
	t.Parallel()
	encodeAt := time.Unix(1700000100, 0)
	thumbAt := time.Unix(1700000200, 0)
	tsAt := time.Unix(1700000300, 0)
	deleteAt := time.Unix(1700000400, 0)
	mediaAt := time.Unix(1700000500, 0)
	scratchAt := time.Unix(1700000600, 0)
	fake := &fakeReconcileMarkers{
		loops: map[string]map[string]time.Time{
			store.LoopReconcilePass: {
				store.ReconcilePassEncode:    encodeAt,
				store.ReconcilePassThumbnail: thumbAt,
				store.ReconcilePassTSScan:    tsAt,
			},
			store.LoopDeleteReconcile: {
				"fs-a":  deleteAt,
				"s3-x":  deleteAt.Add(time.Hour), // 設定に無い置き場。出さない
				"fs-zz": deleteAt.Add(time.Hour), // 設定に無い置き場。出さない
			},
		},
		storage: []store.StorageSync{
			{Store: "fs-a", Root: "media", ObservedAt: mediaAt},
			{Store: "fs-a", Root: "scratch", ObservedAt: scratchAt},
			{Store: "s3-x", Root: "media", ObservedAt: mediaAt.Add(time.Hour)},
		},
	}
	c := NewReconcileMarkerCollector(fake, fake, []string{"fs-a", "fs-b"}, []string{"fs-a", "fs-b"})
	got := collectReconcileSeries(t, c)

	want := map[string]float64{
		"rokuban_encode_reconcile_last_success_timestamp_seconds":                  float64(encodeAt.Unix()),
		"rokuban_thumbnail_reconcile_last_success_timestamp_seconds":               float64(thumbAt.Unix()),
		"rokuban_ts_scan_reconcile_last_success_timestamp_seconds":                 float64(tsAt.Unix()),
		"rokuban_delete_reconcile_last_success_timestamp_seconds{store=fs-a}":      float64(deleteAt.Unix()),
		"rokuban_delete_reconcile_last_success_timestamp_seconds{store=fs-b}":      0,
		"rokuban_storage_root_observed_timestamp_seconds{root=media,store=fs-a}":   float64(mediaAt.Unix()),
		"rokuban_storage_root_observed_timestamp_seconds{root=scratch,store=fs-a}": float64(scratchAt.Unix()),
		"rokuban_storage_root_observed_timestamp_seconds{root=media,store=fs-b}":   0,
	}
	assertSeries(t, got, want)
	for key := range got {
		if strings.Contains(key, "s3-x") || strings.Contains(key, "fs-zz") {
			t.Errorf("series %q for a store outside the configured FS stores, want omitted", key)
		}
	}
}

// 1 つの読み取りが失敗したとき、その表の系列だけが落ち、他の系列は残り、
// 失敗した表の source のエラーカウンタだけが進む。0 に置き換えない。
func TestReconcileMarkerCollector_ReadFailureDropsOnlyItsSeries(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	at := time.Unix(1700000700, 0)
	cases := []struct {
		name   string
		source string
		failed []string // 落ちるべき系列の名前
		kept   []string // 残るべき系列の名前
	}{
		{
			name: "reconcile pass", source: reconcileMarkerSourcePass,
			failed: []string{"rokuban_encode_reconcile_last_success_timestamp_seconds", "rokuban_thumbnail_reconcile_last_success_timestamp_seconds", "rokuban_ts_scan_reconcile_last_success_timestamp_seconds"},
			kept:   []string{"rokuban_delete_reconcile_last_success_timestamp_seconds", "rokuban_storage_root_observed_timestamp_seconds"},
		},
		{
			name: "delete reconcile", source: reconcileMarkerSourceDelete,
			failed: []string{"rokuban_delete_reconcile_last_success_timestamp_seconds"},
			kept:   []string{"rokuban_encode_reconcile_last_success_timestamp_seconds", "rokuban_storage_root_observed_timestamp_seconds"},
		},
		{
			name: "storage sync", source: reconcileMarkerSourceStorage,
			failed: []string{"rokuban_storage_root_observed_timestamp_seconds"},
			kept:   []string{"rokuban_encode_reconcile_last_success_timestamp_seconds", "rokuban_delete_reconcile_last_success_timestamp_seconds"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeReconcileMarkers{
				loops: map[string]map[string]time.Time{
					store.LoopReconcilePass:   {store.ReconcilePassEncode: at},
					store.LoopDeleteReconcile: {"fs-a": at},
				},
				storage: []store.StorageSync{{Store: "fs-a", Root: "media", ObservedAt: at}},
				err:     map[string]error{tc.source: errBoom},
			}
			c := NewReconcileMarkerCollector(fake, fake, []string{"fs-a"}, []string{"fs-a"})
			got := collectReconcileSeries(t, c)

			for _, name := range tc.failed {
				for key := range got {
					if strings.HasPrefix(key, name) {
						t.Errorf("series %q present after a failed %s read, want dropped (not zeroed)", key, tc.name)
					}
				}
			}
			for _, name := range tc.kept {
				found := false
				for key := range got {
					if strings.HasPrefix(key, name) {
						found = true
					}
				}
				if !found {
					t.Errorf("series %q missing after a failed %s read, want kept", name, tc.name)
				}
			}
			counter := "rokuban_reconcile_marker_scrape_errors_total{source=" + tc.source + "}"
			if got[counter] != 1 {
				t.Errorf("%s = %v, want 1", counter, got[counter])
			}
			for _, other := range []string{reconcileMarkerSourcePass, reconcileMarkerSourceDelete, reconcileMarkerSourceStorage} {
				if other == tc.source {
					continue
				}
				if v := got["rokuban_reconcile_marker_scrape_errors_total{source="+other+"}"]; v != 0 {
					t.Errorf("error counter for %s = %v, want 0", other, v)
				}
			}
		})
	}
}

func assertSeries(t *testing.T, got, want map[string]float64) {
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

func TestReconcileMarkerS3Series(t *testing.T) {
	for _, s3 := range []bool{false, true} {
		t.Run(fmt.Sprint(s3), func(t *testing.T) {
			deletes := []string{"fs-a"}
			if s3 {
				deletes = append(deletes, "s3-a")
			}
			fake := &fakeReconcileMarkers{loops: map[string]map[string]time.Time{
				store.LoopDeleteReconcile: {"s3-a": time.Unix(123, 0)},
			}}
			c := NewReconcileMarkerCollector(fake, fake, []string{"fs-a"}, deletes)
			for _, want := range []float64{123, 0} {
				if want == 0 {
					delete(fake.loops[store.LoopDeleteReconcile], "s3-a")
				}
				got := collectReconcileSeries(t, c)
				value, ok := got["rokuban_delete_reconcile_last_success_timestamp_seconds{store=s3-a}"]
				if ok != s3 || (ok && value != want) {
					t.Fatalf("S3 delete series = %v, present %v; want %v, present %v", value, ok, want, s3)
				}
				if _, ok := got["rokuban_storage_root_observed_timestamp_seconds{root=media,store=s3-a}"]; ok {
					t.Fatal("S3 has filesystem storage series")
				}
				if _, ok := got["rokuban_delete_reconcile_last_success_timestamp_seconds{store=fs-a}"]; !ok {
					t.Fatal("FS series missing")
				}
			}
		})
	}
}
