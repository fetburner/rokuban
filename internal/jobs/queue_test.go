package jobs

import (
	"reflect"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/config"
)

func TestAllQueueNames(t *testing.T) {
	if got, want := AllQueueNames(), []string{
		"cleanup",
		"default",
		"encode",
		"epg",
		"ingest",
		"reconciler",
		"ruler",
		"storage",
		"thumbnail",
		"watcher",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("AllQueueNames() = %#v, want %#v", got, want)
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
		{"encode/thumbnail/cleanup/ruler only excludes site-bound queues", []string{EncodeQueue, ThumbnailQueue, CleanupQueue, RulerQueue}, false},
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

// qualifyQueueName は base_site の形にする。空文字列 site は db.DefaultSite に
// 解決する（verifySite / DeleteReconcileWorker.Work と同じ規約。issue #185）。
func TestQualifyQueueName(t *testing.T) {
	tests := []struct {
		name string
		base string
		site string
		want string
	}{
		{"basic", IngestQueue, "tokyo", "ingest_tokyo"},
		{"empty site resolves to db.DefaultSite", EpgQueue, "", "epg_default"},
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
		{"default is NOT qualified", river.QueueDefault, "tokyo", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhysicalQueueName(tt.logical, tt.boundSite); got != tt.want {
				t.Errorf("PhysicalQueueName(%q, %q) = %q, want %q", tt.logical, tt.boundSite, got, tt.want)
			}
		})
	}
}

// TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen は、
// config.MirakcSiteNameMaxLen まで許した site 名を qualifyQueueName で修飾しても
// RiverQueueNameMaxLen を超えないことを機械的に固定する。この関係の両端
// （siteBoundQueueNames・qualifyQueueName・RiverQueueNameMaxLen）はすべて
// internal/jobs 自身の契約なので、テストも jobs 側に置く --- config は
// MirakcSiteNameMaxLen の根拠としてこの契約を import するだけで、逆方向
// （jobs → config）の import は発生しない。site 名の長さ検査は
// config.validateSiteName の 1 本だけなので（worker 側の重複検査は無くした）、
// この関係が壊れると config のロード時検査を通った site 名が qualifyQueueName で
// 64 文字を超える。それを渡した先は worker ロールを持つプロセスなら起動時
// （river.NewClient → Config.validate → `queueConfig.validate` が
// river@v0.47.0 client.go の `validateQueueName` を呼ぶ）に落ち、insert-only
// クライアント（`rokuban enqueue` 等）では Insert 時（river@v0.47.0 client.go の
// `validateQueueName`）に初めて落ちる。
//
// siteBoundQueueNames のどの論理名についても、site 名を
// config.MirakcSiteNameMaxLen まで許してキュー修飾しても RiverQueueNameMaxLen を
// 超えないことを検査する。破る典型は siteBoundQueueNames に `reconciler` より
// 長い論理名を足す、または config.MirakcSiteNameMaxLen を大きくすること。
func TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen(t *testing.T) {
	maxLenSite := strings.Repeat("a", config.MirakcSiteNameMaxLen)
	for _, base := range siteBoundQueueNames {
		name := qualifyQueueName(base, maxLenSite)
		if len(name) > RiverQueueNameMaxLen {
			t.Errorf("qualifyQueueName(%q, <%d-char site>) = %q (%d chars) exceeds RiverQueueNameMaxLen(%d)",
				base, len(maxLenSite), name, len(name), RiverQueueNameMaxLen)
		}
	}
}
