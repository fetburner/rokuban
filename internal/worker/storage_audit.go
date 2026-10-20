// storage_audit.go はストレージ配置を監査し、孤児と実体無しの観測を記録する。
// 読む表は media_assets、orphan_files、missing_media_assets。書く表は orphan_files と
// missing_media_assets。
//
// 走査の入口は walkMediaFiles だけである。孤児と実体無しの監査は、1 パスに 1 回の走査結果
// （seenOnDisk）を共有する。監査ごとに走査すると、片方だけが見た窓で誤判定する。
// 走査の回数は、TestDeleteReconcileWorker_WalksMediaDirOncePerPass が walkMediaFiles の
// 呼び出し回数で検査する。walkMediaFiles を通らない走査はこのテストでは検出できない。
// だから入口を walkMediaFiles に限る約束がある。
//
// 候補は監査が orphan_files に書く。unlink の後に行を消すのは delete_execute.go である。
// エイジング済みの公開前孤児は、名前の分類と mtime の再確認を経て、削除候補として
// 選定に差し出す。
//
// ファイル操作は MediaDir の走査（読み取りのみ）と、公開前孤児の候補に対する stat だけで、
// flock・unlink はしない。ファイル操作の全体表は delete_execute.go の冒頭にある。

package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// reconcileDeleteObservations は孤児と欠損アセットを同じディスク走査結果で観測する。
//
// 孤児候補の記録/解除はファイルを消さないので、ブレーカーとは無関係に毎回行う。
// 同じ 1 回の走査結果（seenOnDisk）を「active なのに実体が無い」検出
// （逆方向。issue #343）にも使う --- 2 回目の全量ディレクトリ走査を避ける。
//
// 疑わしいパス（全損シグネチャ）では記録そのものを見送っているので、
// 報告（Warn ログ・rokuban_media_assets_missing）も合わせて止める。
// ここで無条件に呼ぶと、reconcileMissingAssets が今パスの記録を止めた
// 一方で reportAgedMissingAssets が「前回までに確認済みだった候補」を
// 毎パス出し続けてしまい、metrics.go / docs/operations/monitoring.md が
// 約束する「凍結する」が実装のどこにも実在しない記述になる
// （疑わしい間はゲージを含む報告そのものを止める、という設計判断）。
func (w *DeleteReconcileWorker) reconcileDeleteObservations(ctx context.Context, orphanMTimeGrace, orphanAge, missingAssetAge time.Duration) error {
	seenOnDisk, err := w.reconcileOrphanCandidates(ctx, orphanMTimeGrace)
	if err != nil {
		return fmt.Errorf("reconciling orphan candidates: %w", err)
	}
	suspected, err := w.reconcileMissingAssets(ctx, seenOnDisk)
	if err != nil {
		return fmt.Errorf("reconciling missing assets: %w", err)
	}
	if !suspected {
		if err := w.reportAgedOrphanFiles(ctx, orphanAge); err != nil {
			return fmt.Errorf("reporting aged canonical orphan files: %w", err)
		}
		if err := w.reportAgedMissingAssets(ctx, missingAssetAge); err != nil {
			return fmt.Errorf("reporting aged missing assets: %w", err)
		}
	}
	return nil
}

// reconcileOrphanCandidates は MediaDir を走査し、media_assets のどの行からも
// 参照されていないファイルを孤児候補として orphan_files に記録する。
// mtime が新しいファイル（OrphanMTimeGrace 以内）は候補にしない。
// 既に孤児でなくなった（登録された、またはファイルが消えた）行は掃除する。
// ファイルは一切消さない（記録のみ）。
//
// 戻り値はこの 1 回の走査で実際にディスク上で観測した全 rel_path の集合
// （孤児かどうかを問わない）。reconcileMissingAssets が同じ走査結果を使って
// 逆方向（active なのに実体が無い行）を検出するため（issue #343。2 回目の
// 全量ディレクトリ走査を避ける）。
//
// **この mtime 猶予は、追従 ingest が番組長のあいだ書き続ける record 固有 temp を
// 回収しないことの根拠でもある。** 録画中の temp は書き込みのたびに mtime が
// 新しくなるので候補にならない。猶予を「正常系の ingest が数時間で完結する」
// 前提から短くすると、録画中の書きかけ temp が孤児として aging され、生きて
// いる転送の足元からファイルが消える（unlink 済み inode への書き込みは成功
// し続けるので、失敗は commit の rename で初めて現れる）。
func (w *DeleteReconcileWorker) reconcileOrphanCandidates(ctx context.Context, mtimeGrace time.Duration) (map[string]struct{}, error) {
	known, err := w.Storage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing known rel paths: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
		// VOD 字幕は media_assets の独立行を持たないため、encoded の隣接
		// サイドカーも既知パスとして孤児判定から除外する。media_assets の
		// tombstone は物理削除完了まで残るので、削除途中の .vtt を誤って
		// 候補にすることもない。
		switch strings.ToLower(filepath.Ext(k)) {
		case ".mp4", ".mkv":
			if sidecar, pathErr := mediapath.SubtitleSibling(k); pathErr == nil {
				knownSet[sidecar] = struct{}{}
			}
		}
	}

	mtimeCutoff := time.Now().Add(-mtimeGrace)
	seenOnDisk := make(map[string]struct{})
	candidates := make(map[string]struct{})
	if err := walkMediaFiles(w.MediaDir, func(relPath string, info fs.FileInfo) {
		seenOnDisk[relPath] = struct{}{}
		if _, ok := knownSet[relPath]; ok {
			return
		}
		if info.ModTime().After(mtimeCutoff) {
			return
		}
		candidates[relPath] = struct{}{}
	}); err != nil {
		return nil, fmt.Errorf("walking media dir: %w", err)
	}

	for relPath := range candidates {
		if err := w.Storage.UpsertOrphanFile(ctx, relPath); err != nil {
			return nil, fmt.Errorf("recording orphan candidate %q: %w", relPath, err)
		}
	}

	existing, err := w.Storage.ListAllOrphanFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing orphan files: %w", err)
	}
	for _, o := range existing {
		if _, stillCandidate := candidates[o.RelPath]; stillCandidate {
			continue
		}
		// もう孤児候補でない（media_assets に登録された、mtime が新しくなった
		// はずはないが再走査で見えなくなった＝ファイルが消えた等）。掃除する。
		if err := w.Storage.DeleteOrphanFile(ctx, o.RelPath); err != nil {
			return nil, fmt.Errorf("clearing stale orphan record %q: %w", o.RelPath, err)
		}
	}
	return seenOnDisk, nil
}

// reconcileMissingAssets は orphan 検出と同じ 1 回の走査結果（seenOnDisk）を
// 使い、state='active' な media_assets のうちディスク上で観測されなかった
// 行を missing_media_assets に記録する（issue #343、孤児回収の逆方向）。
// ファイルを消さず、media_assets も一切書き換えない（記録のみ）。
//
// 走査を逆向きに再利用すると walkMediaFiles の除外（catalog.Subdir の
// SkipDir）の誤りの向きが反転する: 孤児方向では除外されたパスは「孤児候補に
// しない」＝削除しない側に倒れるが、逆方向では除外されたパスの資産が
// 恒久的に「実体無し」と誤報される（15 分ごとの Warn + ゲージが下がらない。
// 削除はしないので被害は騒音のみ）。今日は catalog/ がトップレベルの予約
// ディレクトリなので該当する rel_path は存在しないが（docs/storage/contract.md
// §5）、walkMediaFiles に除外を足すときはこちら側の誤報を先に確認する。
// 除外と同型の穴が「走査が降りないディレクトリ」の側にもある --- filepath.Walk
// は symlink を辿らないので、ディレクトリ成分が symlink の構成では配下の資産が
// 同じ形で誤報される（walkMediaFiles の doc コメント。未検証）。
//
// reconcileOrphanCandidates は走査より前に ListAllMediaAssetRelPaths を読むが、
// ここでの ListActiveMediaAssets は走査より後に読む（呼び出し順序どおり）。
// そのため走査の途中でコミットされた資産は、この 1 パスでは一時的に
// 「実体無し」の偽候補になりうる —— この窓も MissingAssetAge のエイジングが
// 埋める（次パスでファイルが観測されれば候補は掃除され、確認済みとして
// 報告される前に消える）。
//
// マウントが落ちている・空マウントのときに全 active 行を「実体無し」と
// 報告して騒がないよう、seenOnDisk が空（この走査で 1 件もファイルを
// 観測できなかった）のに active な行が存在するケースを形で検知して丸ごと
// 見送る（reconciler の全損シグネチャ breaker.ReconcileTotalLoss と同じ
// 考え方 --- 件数の閾値ではなく形で見る。ただしここは削除を止める
// ブレーカーではなく単なる観測の記録なので、ラッチは持たず今パスの記録を
// 見送るだけでよい）。既存の missing_media_assets 行にも一切触れない ---
// 前回までの確認済み状態をこの疑わしいパスの結果で上書きしないため。
//
// 戻り値の bool は今パスが疑わしいパス（全損シグネチャ発動）だったかを
// 呼び出し元（Work）に伝える。呼び出し元はこれを見て reportAgedMissingAssets
// の呼び出し自体を止める --- 記録を見送りながら報告（Warn ログ・
// rokuban_media_assets_missing ゲージ）だけ続けると、metrics.go /
// docs/operations/monitoring.md が約束する「疑わしい間はゲージが凍結する」が
// 実装のどこにも存在しない記述になる。
//
// # ListActiveMediaAssets に deleteReconcileRowLimit を掛けない理由
//
// このファイルの他のソースは全部 deleteReconcileRowLimit で括っているが、
// ここだけは全件を読む。**この判定は差集合なので、入力を切ると答えが変わる**
// --- 窓の外に落ちた active 行はこのパスの candidates に入らず、下の掃除
// ループが「もう候補でない」と見なして既存の missing_media_assets 行を消して
// しまう。first_seen が毎パス失われるので、active が窓を超えている系では
// エイジングが永久に完了せず何も報告されない（他のソースは「拾えた分だけ
// 消す」で正しさが保たれるので窓で足りる。差集合ではないため）。
//
// 作業量の上限は候補数（実体が無い行の数）であって active 全件ではない ---
// 健全な系では候補 0 件で、増える I/O はこの SELECT 1 回だけ。部分マウント
// 障害（walkMediaFiles の doc コメント）では候補が active 全件近くまで伸び、
// 15 分ごとに同じ件数の UPSERT が削除本体と同じジョブの中で走る。それが
// 問題になるなら直し方は入力を切ることではなく UPSERT を 1 文にまとめること。
func (w *DeleteReconcileWorker) reconcileMissingAssets(ctx context.Context, seenOnDisk map[string]struct{}) (bool, error) {
	active, err := w.Storage.ListActiveMediaAssets(ctx)
	if err != nil {
		return false, fmt.Errorf("listing active media assets: %w", err)
	}
	if len(active) == 0 {
		// active が 1 件も無いパスでは下の掃除ループに入らないので、残っている
		// missing_media_assets 行はこのパスでは掃除されない。報告側
		// （ListAgedMissingMediaAssets）が state='active' で弾くので害は無く、
		// active が 1 件でもあるパスで回収される（意図的）。
		return false, nil
	}
	if len(seenOnDisk) == 0 {
		slog.Warn("delete_reconcile: filesystem walk observed zero files while active media_assets rows exist; suspecting a storage mount failure, skipping missing-asset check for this pass",
			"active_assets", len(active))
		metrics.MissingAssetScanSuspectedStorageFailure.Inc()
		return true, nil
	}

	candidates := make(map[int64]struct{})
	for _, a := range active {
		if _, ok := seenOnDisk[a.RelPath]; ok {
			continue
		}
		candidates[a.ID] = struct{}{}
		if err := w.Storage.UpsertMissingMediaAsset(ctx, a.ID); err != nil {
			return false, fmt.Errorf("recording missing-asset candidate %d: %w", a.ID, err)
		}
	}

	existingIDs, err := w.Storage.ListAllMissingMediaAssetIDs(ctx)
	if err != nil {
		return false, fmt.Errorf("listing missing-asset candidates: %w", err)
	}
	for _, id := range existingIDs {
		if _, stillCandidate := candidates[id]; stillCandidate {
			continue
		}
		// もう実体無し候補でない（ファイルが見つかった、またはこの資産自体が
		// もう active でなくなった等）。掃除する。
		if err := w.Storage.DeleteMissingMediaAsset(ctx, id); err != nil {
			return false, fmt.Errorf("clearing stale missing-asset record %d: %w", id, err)
		}
	}
	return false, nil
}

// reportAgedMissingAssets は missing_media_assets のうち first_seen が age を
// 超えて連続して記録されている（単発の走査揺れではない）行を、確認済みの
// 異常として Warn ログとメトリクスに出す（issue #343）。media_assets /
// missing_media_assets のどちらも書き換えない --- 自動削除は行わない
// （「ファイルが無い」は削除の必要条件であって十分条件ではない。
// docs/storage/retention.md §7「孤児回収の逆」）。
//
// 個別 Warn ログは missingAssetLogBudget 件で打ち切り、超過分は件数だけを
// 載せた 1 行（属性 logged / and_more）にまとめる --- 対象が解消するまで
// delete_reconcile の定期パスごとに同じ全件が再送され続けるため
// （件数自体は Reset 後の MediaAssetsMissing ゲージが持つので、ログは同定
// だけを担えばよい）。
func (w *DeleteReconcileWorker) reportAgedMissingAssets(ctx context.Context, age time.Duration) error {
	aged, err := w.Storage.ListAgedMissingMediaAssets(ctx, age)
	if err != nil {
		return fmt.Errorf("listing aged missing-asset candidates: %w", err)
	}

	counts := make(map[string]int, 3)
	for i, a := range aged {
		counts[a.Kind]++
		if i < missingAssetLogBudget {
			slog.Warn("delete_reconcile: active media asset has no file on disk",
				"media_asset_id", a.ID, "recording_id", a.RecordingID, "rel_path", a.RelPath,
				"kind", a.Kind, "first_seen", a.FirstSeen)
		}
	}
	if len(aged) > missingAssetLogBudget {
		slog.Warn("delete_reconcile: suppressing further missing-asset log lines this pass",
			"logged", missingAssetLogBudget, "and_more", len(aged)-missingAssetLogBudget)
	}

	// EncodeReconcileUnsatisfiable と同じパターン: Reset してから現在の
	// パスで見た kind だけ Set する。該当 0 件の kind はラベルの系列自体が
	// 消える（0 を出さない）。
	metrics.MediaAssetsMissing.Reset()
	for kind, n := range counts {
		metrics.MediaAssetsMissing.WithLabelValues(kind).Set(float64(n))
	}
	return nil
}

type orphanCleanupKind uint8

const (
	orphanCleanupCanonical orphanCleanupKind = iota
	orphanCleanupIngestTemp
	orphanCleanupIngestCheckpoint
	orphanCleanupStaging
)

// classifyOrphanCleanup はファイル名から「公開点に達していない」と積極的に示せる種類を返す。
// canonical 名は DB に行が無いだけでは公開前と判断できないため、自動削除しない。
func classifyOrphanCleanup(relPath string) orphanCleanupKind {
	base := filepath.Base(relPath)
	switch {
	case isIngestCheckpointFile(base):
		return orphanCleanupIngestCheckpoint
	case mediapath.IsIngestTempFile(base):
		return orphanCleanupIngestTemp
	case mediapath.IsEncodeTempFile(base), mediapath.IsGeneratedAssetTempFile(base):
		return orphanCleanupStaging
	default:
		return orphanCleanupCanonical
	}
}

// agedUnpublishedOrphans はエイジング済み孤児のうち、名前が公開前を示すものだけを
// 自動削除候補として返す。canonical 名は報告側に残す。
func (w *DeleteReconcileWorker) agedUnpublishedOrphans(ctx context.Context, age, mtimeGrace time.Duration) ([]string, error) {
	agedPaths, err := w.Storage.ListAgedOrphanFiles(ctx, age)
	if err != nil {
		return nil, fmt.Errorf("listing aged orphan files: %w", err)
	}
	if len(agedPaths) == 0 {
		return nil, nil
	}

	known, err := w.Storage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing known rel paths: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
	}

	mtimeCutoff := time.Now().Add(-mtimeGrace)
	verified := make([]string, 0, len(agedPaths))
	for _, relPath := range agedPaths {
		kind := classifyOrphanCleanup(relPath)
		if kind == orphanCleanupCanonical {
			continue
		}
		if _, ok := knownSet[relPath]; ok {
			// 登録され直っていた。孤児ではなくなったので記録から外す。
			if err := w.Storage.DeleteOrphanFile(ctx, relPath); err != nil {
				return nil, fmt.Errorf("clearing reclaimed orphan record %q: %w", relPath, err)
			}
			continue
		}
		path, err := mediapath.Resolve(w.MediaDir, relPath)
		if err != nil {
			if err := w.Storage.DeleteOrphanFile(ctx, relPath); err != nil {
				return nil, fmt.Errorf("clearing invalid orphan record %q: %w", relPath, err)
			}
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				if err := w.Storage.DeleteOrphanFile(ctx, relPath); err != nil {
					return nil, fmt.Errorf("clearing vanished orphan record %q: %w", relPath, err)
				}
				continue
			}
			return nil, fmt.Errorf("statting orphan file %q: %w", relPath, err)
		}
		if info.ModTime().After(mtimeCutoff) {
			// mtime が更新されていた（誰かが触った）。安全側で見送る。
			continue
		}
		verified = append(verified, relPath)
	}
	return verified, nil
}

// reportAgedOrphanFiles はエイジング済みの canonical 孤児を Warn とゲージで報告する。
// orphan_files は reconcile が観測した候補を持つ。公開前を示す予約名は自動回収対象なので
// 報告から除き、canonical 名は人が調査・削除するまで記録を保持する。
// 登録済み rel_path の行は、同じ reconcile パスで先に走る reconcileOrphanCandidates が消している前提で、ここでは再確認しない。
func (w *DeleteReconcileWorker) reportAgedOrphanFiles(ctx context.Context, age time.Duration) error {
	agedPaths, err := w.Storage.ListAgedOrphanFiles(ctx, age)
	if err != nil {
		return fmt.Errorf("listing aged orphan files: %w", err)
	}
	if len(agedPaths) == 0 {
		metrics.OrphanFilesUnresolved.Set(0)
		return nil
	}

	unresolved := 0
	for _, relPath := range agedPaths {
		if classifyOrphanCleanup(relPath) != orphanCleanupCanonical {
			continue
		}
		unresolved++
		if unresolved <= orphanFileLogBudget {
			slog.Warn("delete_reconcile: aged canonical orphan needs review", "rel_path", relPath)
		}
	}
	if unresolved > orphanFileLogBudget {
		slog.Warn("delete_reconcile: suppressing further canonical-orphan log lines this pass",
			"logged", orphanFileLogBudget, "and_more", unresolved-orphanFileLogBudget)
	}
	metrics.OrphanFilesUnresolved.Set(float64(unresolved))
	return nil
}

// beforeWalkMediaFiles は walkMediaFiles の先頭に置くテスト用の同期点で、走査の入口の呼び出し回数を数える。
// 本番では何もしない。
var beforeWalkMediaFiles = func() {}

// walkMediaFiles は mediaDir 配下の通常ファイルを列挙する。catalog.Subdir
// （災害復旧用メタデータ）と .rokuban-locks（調停用 metadata）は走査から除く。
// mediaDir 自体が symlink の場合は、走査 root・catalogDir・rel_path の基準を
// 同じ実体パスに揃えるため、走査前に解決する。
//
// mediaDir が存在しない場合は従来どおり 0 件・エラー無しで返す ---
// reconcileMissingAssets の全損シグネチャ（seenOnDisk が空）がこれを拾って
// 専用の見送り経路に入れるため、ここで error にすると全損検知の入口を
// 迂回してしまう（#671 コメント参照）。権限エラーや symlink ループ等
// 不在以外の解決失敗、および解決後がディレクトリでない場合は error にする
// --- 後者が閉じるのは "." という幻の rel_path が seenOnDisk / orphan_files
// に混入する経路。
//
// 除外を足すときは reconcileMissingAssets の doc コメントを読む --- この結果は
// 孤児方向と実体無し方向の両方が使い、除外の誤りの向きが二者で反対になる。
//
// filepath.Walk は symlink を辿らないので、mediaDir 配下のディレクトリ成分が
// symlink になっている構成（`media/sites -> /mnt/nas/sites` のような合成）では
// そのサブツリーに降りない。ingest / streamer は字句解決（mediapath.Resolve）
// なのでそこへ書けて読めるが、走査からは見えない --- 除外（SkipDir）と同型の
// 穴で、こちらは設定だけで作れる。孤児方向では安全側（消さない）に倒れ、
// 実体無し方向では配下の資産が恒久的に誤報される（未検証。symlink を張った
// 構成でのテストは無い）。symlink を辿る形にはしていない: 循環と、
// mediaDir の外を指す symlink の扱い（rel_path が mediaDir の外のファイルを
// 指すことになる）を先に決める必要があり、この検出器の範囲を超える。
func walkMediaFiles(mediaDir string, fn func(relPath string, info fs.FileInfo)) error {
	beforeWalkMediaFiles()
	resolvedMediaDir, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("resolving media dir %q: %w", mediaDir, err)
	}
	rootInfo, err := os.Stat(resolvedMediaDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stating resolved media dir %q: %w", resolvedMediaDir, err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("resolved media dir %q is not a directory", resolvedMediaDir)
	}

	catalogDir := filepath.Join(resolvedMediaDir, catalog.Subdir)
	lockDir := filepath.Join(resolvedMediaDir, mediapath.MediaRelPathLockDirName)
	return filepath.Walk(resolvedMediaDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if path == lockDir {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if path == catalogDir {
				return filepath.SkipDir
			}
			return nil
		}
		// 旧形式は rolling upgrade 中の旧 worker と共有する可能性があるため、
		// orphan として unlink しない。新形式は root の lock directory ごと上で除外する。
		if mediapath.IsMediaRelPathLockFile(info.Name()) {
			return nil
		}
		relPath, err := filepath.Rel(resolvedMediaDir, path)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(relPath), info)
		return nil
	})
}
