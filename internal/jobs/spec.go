package jobs

import (
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
)

// 各締切の根拠。jobq.Spec.Timeout は Work の context を DeadlineExceeded で終了させ、
// プロセス死後の再配送は締切 + adapter の回収猶予以内に始まる。
// 標準の締切は 1 分である。
// ingest / ruler / reconciler / record_sweep / epg_sync / encode_reconcile / catalog_export /
// delete_reconcile の締切が既定より長いのは、DB の往復・mirakc への HTTP・ファイル I/O が
// 件数に比例して積み上がり、1 分では足りなくなりうるからである。
// ffmpeg を起動しないジョブ（reconcile 系）は encode のような長時間締切を要しない。
const (
	defaultJobTimeout = time.Minute
	// ingest は転送を区切りごとに snooze して継続するので、締切は録画全体の長さや転送速度で
	// 決まらない。5 分にしたのは、jobq の契約ではプロセス死後の再配送が締切 + adapter の
	// 回収猶予以内だからである。締切を長くするとプロセス死後の再配送上限も長くなる。
	// 追従の無進捗検知は従来どおり StallTimeout が担う。
	ingestJobTimeout = 5 * time.Minute
	// epg_sync は mirakc への GET 2 回と Postgres 射影を含む全量同期 1 パスの上限。
	epgSyncJobTimeout = 10 * time.Minute
	// tuner_sync は数本〜十数本を返す /api/tuners だけなので既定と同じ。無応答のまま
	// ジョブを掴み続けないよう上限は明示する。
	tunerSyncJobTimeout = time.Minute
	// ruler は mirakc に触れない純粋な Postgres 処理だが、ルールごとに DB 往復があり、
	// 数十〜数百ルールと GC で既定の 1 分を超えうる。
	rulerPassJobTimeout = 5 * time.Minute
	// reconciler は mirakc への schedules 全量 GET と差分の POST / DELETE を含み、
	// ネットワーク往復が支配的になりうるので epg_sync と同じ姿勢で長めに置く。
	reconcilePassJobTimeout = 10 * time.Minute
	// record_sweep は records 全量 GET と record ごとの DB トランザクションを含む。
	// record のバイト転送は別の ingest が担う。
	recordSweepJobTimeout = 10 * time.Minute
	// thumbnail は大容量 TS への入力シークでも 5 分あれば足りる（未検証）。
	thumbnailJobTimeout = 5 * time.Minute
	// seek_tiles は合成 TS で測った 45 ms/枚を 1080 枚へ外挿すると約 49 秒である。
	// 外挿も実録画・J4125 での生成時間も未検証（測定は 10 分・60 枚）。
	seekTilesJobTimeout = 15 * time.Minute
	// thumbnail_reconcile は候補の読み取りと投入だけを行う（抽出は ThumbnailWorker）。
	thumbnailReconcileJobTimeout = 5 * time.Minute
	cmDetectReconcileJobTimeout  = 5 * time.Minute
	// encode_reconcile は ffmpeg を起動しないが、不足している (recording_id, profile) ごとに
	// 投入が発生し、候補数とプロファイル数に比例して DB の処理が積み上がる。
	encodeReconcileJobTimeout = 5 * time.Minute
	// delete_reconcile は 1 パスでファイル走査と物理 unlink を多数行う。
	deleteReconcileJobTimeout = 10 * time.Minute
	// catalog_export は DB 読みと JSON 書き込みと刈り取りで、対象は数 MB 規模である。
	catalogExportJobTimeout = 5 * time.Minute
	// storage_sync は root ごとの statfs と DB upsert 高々 2 回で、tuner_sync と同じ理由で
	// 既定を明示する。
	storageSyncJobTimeout = time.Minute
)

// EncodeUnknownDurationTimeout is the execution limit for an encode job without a saved
// duration-based deadline, including older queued arguments. Spec.Timeout is Work's context
// deadline, so a short estimate would fail every long encode. Slower redelivery after a process
// dies is the cheaper error.
const EncodeUnknownDurationTimeout = 12 * time.Hour

func fixedTimeout(timeout time.Duration) func(jobq.Args) time.Duration {
	return func(jobq.Args) time.Duration { return timeout }
}

func spec(queue string, timeout func(jobq.Args) time.Duration, maxAttempts int, coalesce bool, priority int) jobq.Spec {
	if priority == 0 {
		priority = 1
	}
	return jobq.Spec{
		Queue:       queue,
		Timeout:     timeout,
		MaxAttempts: maxAttempts,
		Coalesce:    coalesce,
		Priority:    priority,
	}
}

func cmDetectionTimeout(args jobq.Args) time.Duration {
	var durationMs int64
	switch args := args.(type) {
	case CMDetectJobArgs:
		durationMs = args.RecordingDurationMs
	case CMLogoCandidateJobArgs:
		durationMs = args.RecordingDurationMs
	default:
		return 0
	}
	minimum := 30 * time.Minute
	if durationMs <= 0 || durationMs > int64((time.Duration(1<<63-1)/2)/time.Millisecond) {
		return minimum
	}
	timeout := 2 * time.Duration(durationMs) * time.Millisecond
	if timeout < minimum {
		return minimum
	}
	return timeout
}

// Spec は ingest の論理キュー、有限の 5 分締切、合流ヒントを返す。
func (a IngestJobArgs) Spec() jobq.Spec {
	return spec(IngestQueue, fixedTimeout(ingestJobTimeout), 0, true, 1)
}

// Spec は EPG 同期の論理キュー、10 分締切、合流ヒントを返す。
func (a EpgSyncArgs) Spec() jobq.Spec {
	return spec(EpgQueue, fixedTimeout(epgSyncJobTimeout), 0, true, 1)
}

// Spec はチューナー同期の論理キュー、1 分締切、合流ヒントを返す。
func (a TunerSyncArgs) Spec() jobq.Spec {
	return spec(EpgQueue, fixedTimeout(tunerSyncJobTimeout), 0, true, 1)
}

// Spec は ruler パスの論理キュー、5 分締切、合流ヒントを返す。
// Coalesce は排他を保証しない。
func (a RulerPassArgs) Spec() jobq.Spec {
	return spec(RulerQueue, fixedTimeout(rulerPassJobTimeout), 0, true, 1)
}

// Spec は reconciler パスの論理キュー、10 分締切、合流ヒントを返す。
// Queue は論理名のままにし、adapter が site 束縛に応じて物理キュー名を導出する。
func (a ReconcilePassArgs) Spec() jobq.Spec {
	return spec(ReconcilerQueue, fixedTimeout(reconcilePassJobTimeout), 0, true, 1)
}

// Spec は record sweep の論理キュー、10 分締切、合流ヒントを返す。
func (a RecordSweepArgs) Spec() jobq.Spec {
	return spec(RecordSweepQueue, fixedTimeout(recordSweepJobTimeout), 0, true, 1)
}

// Spec は encode の論理キュー、保存済み締切または既定の 12 時間締切、26 回の
// adapter failure 上限、合流ヒントを返す。adapter の failure 数と永続化する
// encode 試行数は別で、上限に達して配信が止まった場合は encode reconcile が再投入する。
func (a EncodeJobArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, func(raw jobq.Args) time.Duration {
		args, ok := raw.(EncodeJobArgs)
		if !ok {
			return 0
		}
		if args.Timeout > 0 {
			return args.Timeout
		}
		return EncodeUnknownDurationTimeout
	}, 26, true, 1)
}

// Spec は thumbnail の論理キュー、5 分締切、合流ヒントを返す。
func (a ThumbnailJobArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(thumbnailJobTimeout), 0, true, 1)
}

// Spec は seek tiles の論理キュー、15 分締切、優先度 4、合流ヒントを返す。
func (a SeekTilesJobArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(seekTilesJobTimeout), 0, true, 4)
}

// Spec は thumbnail reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a ThumbnailReconcileArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(thumbnailReconcileJobTimeout), 0, true, 1)
}

// Spec は CM 検出の論理キュー、録画尺に応じた締切、10 回の adapter 試行上限、
// 合流ヒントを返す。
func (a CMDetectJobArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, cmDetectionTimeout, 10, true, 1)
}

// Spec は CM 検出 reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a CMDetectReconcileArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, fixedTimeout(cmDetectReconcileJobTimeout), 0, true, 1)
}

// Spec は CM ロゴ候補の論理キュー、録画尺に応じた締切、1 回の adapter 試行上限、
// 合流ヒントを返す。
func (a CMLogoCandidateJobArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, cmDetectionTimeout, 1, true, 1)
}

// Spec は分類ルール reconcile の論理キュー、1 分締切を返す。
// Coalesce は無効にし、編集中に投入された各変更を残す。
func (a LabelRuleReconcileArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(defaultJobTimeout), 0, false, 1)
}

// Spec は encode reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a EncodeReconcileArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, fixedTimeout(encodeReconcileJobTimeout), 0, true, 1)
}

// Spec は利用者が要求した encode rebuild の論理キュー、1 分締切、
// 合流ヒントを返す。
func (a EncodeRebuildArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, fixedTimeout(defaultJobTimeout), 0, true, 1)
}

// Spec は delete reconcile の論理キュー、10 分締切、合流ヒントを返す。
func (a DeleteReconcileArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(deleteReconcileJobTimeout), 0, true, 1)
}

// Spec は catalog export の論理キュー、5 分締切、合流ヒントを返す。
func (a CatalogExportArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(catalogExportJobTimeout), 0, true, 1)
}

// Spec は storage sync の論理キュー、1 分締切、合流ヒントを返す。
func (a StorageSyncArgs) Spec() jobq.Spec {
	return spec(StorageQueue, fixedTimeout(storageSyncJobTimeout), 0, true, 1)
}
