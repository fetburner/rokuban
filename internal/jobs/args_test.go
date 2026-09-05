package jobs

import (
	"slices"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// 完了済みのジョブが一意性の判定に入っていないこと。
//
// River の既定（UniqueOptsByStateDefault）は completed を含むため、既定のままだと
// 一度成功した引数のジョブが二度と投入できなくなる。epg_sync は 10 分間隔の
// 定期ジョブなので、これに当たると実質ワンショットになる（実際にそうなっていた）。
func TestInsertOpts_UniqueStatesExcludeFinalized(t *testing.T) {
	tests := []struct {
		name string
		opts river.InsertOpts
	}{
		{"epg_sync", EpgSyncArgs{}.InsertOpts()},
		{"ingest", IngestJobArgs{}.InsertOpts()},
		{"encode", EncodeJobArgs{}.InsertOpts()},
		{"ruler_pass", RulerPassArgs{}.InsertOpts()},
		{"reconcile_pass", ReconcilePassArgs{}.InsertOpts()},
		{"catalog_export", CatalogExportArgs{}.InsertOpts()},
		{"storage_sync", StorageSyncArgs{}.InsertOpts()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := tt.opts.UniqueOpts.ByState
			if len(states) == 0 {
				t.Fatal("ByState が空だと River の既定（completed を含む）が使われる")
			}
			for _, s := range states {
				switch s {
				case rivertype.JobStateCompleted, rivertype.JobStateDiscarded, rivertype.JobStateCancelled:
					t.Errorf("終了状態 %q が一意性の判定に含まれている", s)
				}
			}
			// 同時実行を防ぐ目的は満たしていること
			if !slices.Contains(states, rivertype.JobStateRunning) {
				t.Error("running が含まれていないと同時実行を防げない")
			}
		})
	}
}

// site 単位のキュー（ingest/epg/reconciler/watcher。tuner_sync は epg キューを
// 共有）と cleanup（delete_reconcile/catalog_export）は UniqueOpts.ByQueue: true
// を立てていること。ruler は対象外（キュー名を変えていないので不要。
// PhysicalQueueName のコメント参照）。
//
// 立てないと、キュー名を変える（今回の site 修飾・cleanup への移設）だけで
// 旧キューの残骸が新キューへの Insert を UniqueSkippedAsDuplicate として
// 黙って塞ぐ（UniqueByQueue の doc コメント、issue #185 のレビュー
// 指摘）。この 1 つのテーブルにまとめておくことで、7 種のうち 1 つでも
// ByQueue を書き忘れたときに検出漏れが起きないようにする。
//
// storage_sync だけは事情が違う（キュー名を変えたことがないので、この表が
// 押さえている「リネームで塞がる」失敗はまだ起きえない）。専用の storage
// キューを新設した時点で先に立てておくという選択の記録としてここに置く ---
// 後から `storage` を改名したくなったときに、上記の失敗を踏み直さないため。
func TestInsertOpts_ByQueueForRenamedQueues(t *testing.T) {
	tests := []struct {
		name string
		opts river.InsertOpts
		want bool
	}{
		{"ingest", IngestJobArgs{}.InsertOpts(), true},
		{"epg_sync", EpgSyncArgs{}.InsertOpts(), true},
		{"tuner_sync", TunerSyncArgs{}.InsertOpts(), true},
		{"reconcile_pass", ReconcilePassArgs{}.InsertOpts(), true},
		{"record_sweep", RecordSweepArgs{}.InsertOpts(), true},
		{"delete_reconcile", DeleteReconcileArgs{}.InsertOpts(), true},
		{"catalog_export", CatalogExportArgs{}.InsertOpts(), true},
		{"storage_sync (new queue, set up-front against a future rename)", StorageSyncArgs{}.InsertOpts(), true},
		{"ruler_pass (queue name unchanged, not required)", RulerPassArgs{}.InsertOpts(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.UniqueOpts.ByQueue; got != tt.want {
				t.Errorf("UniqueOpts.ByQueue = %v, want %v", got, tt.want)
			}
		})
	}
}
