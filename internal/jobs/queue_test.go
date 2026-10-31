package jobs

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
)

func TestAllQueueNames(t *testing.T) {
	if got, want := AllQueueNames(), []string{
		"cleanup",
		"cm_detect",
		"default",
		"encode",
		"epg",
		"ingest",
		"original",
		"reconciler",
		"ruler",
		"storage",
		"thumbnail",
		"transfer",
		"ts_scan",
		"watcher",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("AllQueueNames() = %#v, want %#v", got, want)
	}
}

func TestPhysicalStoreQueueNameAndValidation(t *testing.T) {
	if got := PhysicalStoreQueueName("transfer", "tokyo"); got != "transfer_tokyo" {
		t.Fatalf("PhysicalStoreQueueName() = %q, want literal %q", got, "transfer_tokyo")
	}
	if got, err := PhysicalQueueFor(StorageTransferArgs{}); err != nil || got != "transfer_default" {
		t.Fatalf("PhysicalQueueFor(zero transfer args) = (%q, %v), want transfer_default for worker registration", got, err)
	}
	args := StorageTransferArgs{Store: strings.Repeat("a", config.MaxStoreNameLen)}
	if got, err := PhysicalQueueFor(args); err != nil || len(got) != 63 {
		t.Fatalf("PhysicalQueueFor(max store) = (%q, %v), want a 63-character queue", got, err)
	}
	args.Store = "bad_store"
	if _, err := PhysicalQueueFor(args); err == nil {
		t.Fatal("PhysicalQueueFor(invalid store) succeeded, want error")
	}
}

// 置き場で修飾する論理キューの集合をリテラルで固定する。DB だけを触る reconcile（cleanup）と、
// site で修飾するキューは含まれない。投入側（PhysicalQueueFor）・worker 仕様・起動時検査・--once 検査は
// この 1 つの集合を共有する。
func TestStoreBoundQueueNames(t *testing.T) {
	want := []string{"transfer", "encode", "thumbnail", "cm_detect", "ts_scan", "original", "storage"}
	if !reflect.DeepEqual(storeBoundQueueNames, want) {
		t.Errorf("storeBoundQueueNames = %v, want literal %v", storeBoundQueueNames, want)
	}
	for _, logical := range AllQueueNames() {
		if got := IsStoreBound(logical); got != slices.Contains(want, logical) {
			t.Errorf("IsStoreBound(%q) = %v", logical, got)
		}
	}
	if got := StoreBoundQueueCount(nil); got != 7 {
		t.Errorf("StoreBoundQueueCount(all) = %d, want 7", got)
	}
	if got := StoreBoundQueueCount([]string{"encode", "ruler", "cleanup", "ingest"}); got != 1 {
		t.Errorf("StoreBoundQueueCount(encode, ruler, cleanup, ingest) = %d, want 1", got)
	}
}

// 置き場で修飾する論理キューに載る Args は、すべて QueueStore を実装して置き場のキューに積まれる。
// 実装し忘れると PhysicalQueueFor が投入前に拒む。
func TestEveryStoreBoundArgsRoutesToItsStoreQueue(t *testing.T) {
	for _, args := range []jobq.Args{
		EncodeJobArgs{Store: "takamatsu"}, ThumbnailJobArgs{Store: "takamatsu"}, SeekTilesJobArgs{Store: "takamatsu"},
		CMDetectJobArgs{Store: "takamatsu"}, CMLogoCandidateJobArgs{Store: "takamatsu"}, DeleteReconcileArgs{Store: "takamatsu"},
		CatalogExportArgs{Store: "takamatsu"}, StorageSyncArgs{Store: "takamatsu"}, StorageTransferArgs{Store: "takamatsu"},
	} {
		got, err := PhysicalQueueFor(args)
		if err != nil {
			t.Fatalf("PhysicalQueueFor(%T): %v", args, err)
		}
		want := map[string]string{
			"EncodeJobArgs": "encode_takamatsu", "ThumbnailJobArgs": "thumbnail_takamatsu", "SeekTilesJobArgs": "thumbnail_takamatsu",
			"CMDetectJobArgs": "cm_detect_takamatsu", "CMLogoCandidateJobArgs": "cm_detect_takamatsu", "DeleteReconcileArgs": "original_takamatsu",
			"CatalogExportArgs": "original_takamatsu", "StorageSyncArgs": "storage_takamatsu", "StorageTransferArgs": "transfer_takamatsu",
		}[reflect.TypeOf(args).Name()]
		if got != want {
			t.Errorf("PhysicalQueueFor(%T) = %q, want %q", args, got, want)
		}
	}
	// DB だけを触る reconcile は置き場で修飾しない。
	for _, args := range []jobq.Args{EncodeReconcileArgs{}, ThumbnailReconcileArgs{}, CMDetectReconcileArgs{}, StorageTransferReconcileArgs{}, LabelRuleReconcileArgs{}} {
		if got, err := PhysicalQueueFor(args); err != nil || got != "cleanup" {
			t.Errorf("PhysicalQueueFor(%T) = %q, %v; want cleanup", args, got, err)
		}
	}
}

func TestRequiresCMDetectTools(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including CM detection", nil, true},
		{"explicit CM detection", []string{CMDetectQueue}, true},
		{"explicit non-CM queue", []string{ThumbnailQueue}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresCMDetectTools(tt.queues); got != tt.want {
				t.Errorf("RequiresCMDetectTools(%v) = %v, want %v", tt.queues, got, tt.want)
			}
		})
	}
}

// RequiresEncodeTools が worker.queues の絞り込みと連動していること
// （issue #113 決定 C）。既定（空）や encode/thumbnail を明示的に含む場合は
// ffmpeg/ffprobe 検査が必要、それ以外に絞った場合は不要になる。
func TestRequiresEncodeTools(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including encode/thumbnail", nil, true},
		{"explicit encode", []string{EncodeQueue}, true},
		{"explicit thumbnail", []string{ThumbnailQueue}, true},
		{"explicit both", []string{EncodeQueue, ThumbnailQueue}, true},
		{"ingest only excludes encode/thumbnail", []string{IngestQueue}, false},
		{"ruler/reconciler only excludes encode/thumbnail", []string{RulerQueue, ReconcilerQueue}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresEncodeTools(tt.queues); got != tt.want {
				t.Errorf("RequiresEncodeTools(%v) = %v, want %v", tt.queues, got, tt.want)
			}
		})
	}
}

// RequiresSiteBinding は cmd/rokuban が「worker ロールを 0 サイト束縛
// （issue #183 M4-11 の --sites=）で起動してよいか」を判定する唯一の材料。
// ここが誤ると、site 単位のジョブ（ingest/epg/reconciler/watcher）を
// 引く worker が空文字列 site のまま起動し、届いたジョブの site と一致せず
// 全滅して再試行し続ける（ログにも出ない）。
//
// ruler は site 非依存（issue #185 M4-13。#138 の決定表 --- DB のみで mirakc に
// 触れない）なので、ruler だけの購読は 0 サイト束縛を要求しない。
func TestRequiresSiteBinding(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including site-bound ones", nil, true},
		{"explicit ingest", []string{IngestQueue}, true},
		{"explicit epg", []string{EpgQueue}, true},
		{"explicit ruler does not require binding (site-independent, issue #185)", []string{RulerQueue}, false},
		{"explicit reconciler", []string{ReconcilerQueue}, true},
		{"explicit watcher (record_sweep)", []string{RecordSweepQueue}, true},
		{"site-independent media queues do not require binding", []string{EncodeQueue, ThumbnailQueue, TSScanQueue, CleanupQueue, RulerQueue}, false},
		{"explicit storage does not require binding (site-independent, issue #238)", []string{StorageQueue}, false},
		{"encode/thumbnail plus one site-bound queue still requires binding", []string{EncodeQueue, IngestQueue}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresSiteBinding(tt.queues); got != tt.want {
				t.Errorf("RequiresSiteBinding(%v) = %v, want %v", tt.queues, got, tt.want)
			}
		})
	}
}

// この 1 つのテストが、M4-11 が M4-13 に申し送った罠を直接固定する（issue #185
// のコメント）: worker.queues（config）は論理名のままで、RequiresSiteBinding /
// RequiresEncodeTools はその論理名に対して判定する。キュー名を site で修飾する
// 実装が、誤ってこの論理名そのもの（キュー定数や siteBoundQueueNames の要素）を
// 修飾済みの文字列に変えてしまうと、cmd/rokuban.validateSiteBinding が
// worker.queues の値と一致判定できなくなり、0 サイト束縛の worker が site 単位の
// キューを購読できる状態のまま起動時ガードを素通りする。
func TestRequiresSiteBinding_LogicalQueueNamesStayUnqualified(t *testing.T) {
	for _, base := range []string{IngestQueue, EpgQueue, ReconcilerQueue, RecordSweepQueue} {
		if strings.Contains(base, "_") {
			t.Errorf("queue base %q looks site-qualified; worker.queues (config) と "+
				"RequiresSiteBinding/RequiresEncodeTools は論理名（unqualified）を前提にしている", base)
		}
		if !RequiresSiteBinding([]string{base}) {
			t.Errorf("RequiresSiteBinding([%q]) = false, want true (site-bound queue のはず)", base)
		}
	}
}

// qualifyQueueName は base_site の形にする。空文字列 site は store.DefaultSite に
// 解決する（verifySite / DeleteReconcileWorker.Work と同じ規約。issue #185）。
func TestQualifyQueueName(t *testing.T) {
	tests := []struct {
		name string
		base string
		site string
		want string
	}{
		{"basic", IngestQueue, "tokyo", "ingest_tokyo"},
		{"empty site resolves to store.DefaultSite", EpgQueue, "", "epg_default"},
		{"reconciler", ReconcilerQueue, "takamatsu", "reconciler_takamatsu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qualifyQueueName(tt.base, tt.site); got != tt.want {
				t.Errorf("qualifyQueueName(%q, %q) = %q, want %q", tt.base, tt.site, got, tt.want)
			}
		})
	}
}

// PhysicalQueueName は siteBoundQueueNames に含まれる論理名だけ修飾し、
// それ以外（ruler/encode/thumbnail/cleanup/default）はそのまま通す。
// want はすべてリテラルで書く（logical と同じ定数を want にも使うと、
// 「qualify しない」ケースは定数の値が何であっても常に一致してしまい、
// 意図した論理名そのものが正しいかを確認しない。issue #185 のレビュー指摘）。
func TestPhysicalQueueName(t *testing.T) {
	tests := []struct {
		name      string
		logical   string
		boundSite string
		want      string
	}{
		{"ingest gets qualified", IngestQueue, "tokyo", "ingest_tokyo"},
		{"epg gets qualified", EpgQueue, "tokyo", "epg_tokyo"},
		{"reconciler gets qualified", ReconcilerQueue, "tokyo", "reconciler_tokyo"},
		{"watcher (record_sweep) gets qualified", RecordSweepQueue, "tokyo", "watcher_tokyo"},
		{"ruler is NOT qualified (site-independent, issue #185)", RulerQueue, "tokyo", "ruler"},
		{"encode is NOT qualified", EncodeQueue, "tokyo", "encode"},
		{"thumbnail is NOT qualified", ThumbnailQueue, "tokyo", "thumbnail"},
		{"cleanup is NOT qualified", CleanupQueue, "tokyo", "cleanup"},
		{"storage is NOT qualified (site-independent, issue #238)", StorageQueue, "tokyo", "storage"},
		{"default is NOT qualified", "default", "tokyo", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhysicalQueueName(tt.logical, tt.boundSite); got != tt.want {
				t.Errorf("PhysicalQueueName(%q, %q) = %q, want %q", tt.logical, tt.boundSite, got, tt.want)
			}
		})
	}
}

// TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen は、最大長の site 名で作った
// 物理キュー名が jobq の共通上限内に収まることを固定する。siteBoundQueueNames に
// `reconciler` より長い論理名を追加するか、config.MirakcSiteNameMaxLen を大きくすると
// このテストが落ちる。
func TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen(t *testing.T) {
	maxLenSite := strings.Repeat("a", config.MirakcSiteNameMaxLen)
	for _, base := range siteBoundQueueNames {
		name := qualifyQueueName(base, maxLenSite)
		if len(name) > jobq.MaxQueueNameLen {
			t.Errorf("qualifyQueueName(%q, <%d-char site>) = %q (%d chars) exceeds jobq.MaxQueueNameLen(%d)",
				base, len(maxLenSite), name, len(name), jobq.MaxQueueNameLen)
		}
	}
}
