package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/tsstat"
)

const (
	defaultStallTimeout = 30 * time.Second
	maxInJobRetries     = 5
	ingestQueue         = "ingest"

	// connectRetryBaseDelay / connectRetryMaxDelay は StreamRecord への接続が
	// 失敗したときの指数バックオフの下限・上限。mirakc が即座に refuse する状況
	// （再起動直後など）で 6 回のリトライを一瞬で使い切らないようにする。
	// 転送中断（Range 再開）側は stallReader が既に間を置くのでバックオフ不要。
	connectRetryBaseDelay = 200 * time.Millisecond
	connectRetryMaxDelay  = 5 * time.Second
)

// IngestJobArgs は ingest ジョブの引数。mirakc サイトと record ID を指定する。
type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

// Kind は River ジョブの種別名を返す。
func (IngestJobArgs) Kind() string { return "ingest" }

// NewIngestArgs は IngestJobArgs を river.JobArgs として組み立てる。
//
// internal/watcher.Watcher に IngestJobArgs の具体型を注入するための関数値
// （watcher.IngestArgsFunc）として使う。internal/watcher は internal/worker に
// 依存できない（依存すると record_sweep ジョブ経由で循環インポートになる。
// watcher.IngestArgsFunc のコメント参照）ため、呼び出し元（cmd/rokuban と
// RecordSweepWorker）がこの関数を渡す。
func NewIngestArgs(site, recordID string) river.JobArgs {
	return IngestJobArgs{Site: site, RecordID: recordID}
}

// InsertOpts は River ジョブの挿入オプションを返す。
//
// watcher は record-saved イベントと定期の全量突き合わせの両方から同じ record を
// 投入しうるので、ByArgs で一意化して二重取り込みを防ぐ。ByState は
// pendingJobStates に絞る（既定は completed を含むため、一度取り込んだ record を
// 手動で取り直せなくなる。取り込み済みかどうかは media_assets 行が真実であり、
// River のジョブ履歴で表現するものではない）。
//
// Queue は a.Site で修飾する（physicalQueueName、issue #185 M4-13）。ingest は
// mirakc への到達性を要する site 単位の仕事なので、多サイト構成で site A の
// worker が site B の ingest ジョブを掴まないよう、キュー選択の時点で分離する
// （verifySite は届いた後の多重防御。qualifyQueueName のコメント参照 --- 必ず
// physicalQueueName を経由し、直接 qualifyQueueName を呼ばない）。
//
// ByQueue: uniqueByQueue を立てる理由は pendingJobStates 直後の doc コメント
// 参照（キュー名の変更が一意キーに影響しないと、旧キューの残骸が新キューへの
// insert を黙って塞ぐ）。
func (a IngestJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: physicalQueueName(ingestQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// IngestWorker は mirakc からの TS ファイル転送を行う River ワーカー。
type IngestWorker struct {
	river.WorkerDefaults[IngestJobArgs]
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string
	StallTimeout time.Duration

	// Site はこのワーカープロセス自身の site（config.mirakc.site）。Work は
	// これと args.Site を verifySite で照合してから mirakc/FS に触る
	// （issue #139）。空なら db.DefaultSite に解決する（verifySite 参照）。
	Site string
}

// Timeout は River の総時間タイムアウトを無効化する。
//
// ingest は数百 MB〜数十 GB のバイト転送で、所要時間は録画長と回線速度で決まる。
// River の既定（JobTimeoutDefault = 1 分）では実際の録画がまず完走しない。
//
// 総時間で切らない代わりに、進捗が止まったことを stallReader が検知して打ち切る
// （StallTimeout）。「タイムアウトは総時間でなくストール検知」という M1-5-2 の
// 設計はこれが揃って初めて成立する。
func (w *IngestWorker) Timeout(*river.Job[IngestJobArgs]) time.Duration {
	return -1
}

// resolveStallTimeout は設定された StallTimeout があればそれを、なければ既定の
// 30 秒を返す。0 は「未設定」とみなし、ingest.stall_timeout を書かない構成でも
// 既定で動くようにする。
func (w *IngestWorker) resolveStallTimeout() time.Duration {
	if w.StallTimeout == 0 {
		return defaultStallTimeout
	}
	return w.StallTimeout
}

// Work は ingest ジョブを実行する。ストリーム取得・TS 統計収集・DB コミット・エッジ削除を行う。
func (w *IngestWorker) Work(ctx context.Context, job *river.Job[IngestJobArgs]) error {
	args := job.Args
	log := slog.With("site", args.Site, "record_id", args.RecordID)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.IngestDuration.Observe(time.Since(started).Seconds())
		metrics.IngestJobs.WithLabelValues(result).Inc()
	}()

	// mirakc の record id はインスタンススコープ。他サイトのジョブをこの
	// プロセスの mirakc に投げると、別番組をこの recording としてコミットしうる
	// （issue #139）。DB 参照（lookupRecordingID）や mirakc/FS への一切の
	// アクセスより前に照合する。
	if err := verifySite(w.Site, args.Site, ingestQueue); err != nil {
		return err
	}

	stallTimeout := w.resolveStallTimeout()

	recordingID, err := w.lookupRecordingID(ctx, args)
	if err != nil {
		return fmt.Errorf("looking up recording_id: %w", err)
	}

	// 冪等性チェック：この recording_id の original media_asset が既にコミット
	// 済みなら転送をやり直さない。エッジ record の削除（下の DeleteRecord）は
	// 失敗してもログのみで ingest 成功扱いにしている（意図的）ため、mirakc 側に
	// record が残ったまま 5 分後の record_sweep → watcher.processRecord
	// （status=finished）経由で同じ record の ingest ジョブが再投入されうる。
	// pendingJobStates の UniqueOpts は completed を除外するのでこの再投入は
	// 止まらない。ここで止めないと os.Create がコミット済みファイルを 0
	// バイトに切り詰めて全量を再ダウンロードし、streamer は不変条件 3
	// （コミット = DB 行）で既にコミット済みの録画に対して欠けたファイルを
	// 配ることになる。
	alreadyCommitted, err := w.hasOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking existing media_asset: %w", err)
	}
	if alreadyCommitted {
		log.Info("ingest: media_asset already committed, skipping transfer", "recording_id", recordingID)
		// 転送は行っていないが、DB からは成功と区別が付かない状態なので
		// メトリクスも成功として数える。ここでの唯一の残り仕事はエッジ
		// record の削除（下記）の再試行であり、それが完了すれば ingest の
		// 目的（コミット済み・record 削除済み）は満たされている。
		result = "success"
		// 原本があるなら encode の desired−observed も埋める（ヒント。真実は
		// EnqueueMissingEncodes のレベルトリガー判定。issue #65）。
		enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID)
		if _, err := w.MirakcClient.DeleteRecord(ctx, args.RecordID, true); err != nil {
			log.Error("ingest: failed to delete edge record (already committed)", "err", err)
		}
		return nil
	}

	relPath, fullPath, err := w.determineRelPath(ctx, args)
	if err != nil {
		return fmt.Errorf("determining rel_path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", filepath.Dir(fullPath), err)
	}

	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", fullPath, err)
	}
	defer func() { _ = f.Close() }()

	counter := tsstat.NewCounter(f)

	var offset int64
	for attempt := 0; ; attempt++ {
		stallCtx, stallCancel := context.WithCancel(ctx)

		body, _, err := w.MirakcClient.StreamRecord(stallCtx, args.RecordID, offset)
		if err != nil {
			stallCancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt >= maxInJobRetries {
				return fmt.Errorf("streaming record (attempt %d): %w", attempt, err)
			}
			delay := connectRetryDelay(attempt)
			log.Warn("ingest: stream connect failed, retrying", "attempt", attempt, "err", err, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}

		timer := time.AfterFunc(stallTimeout, func() { stallCancel() })
		sr := &stallReader{r: body, timer: timer, d: stallTimeout}
		n, copyErr := io.Copy(counter, sr)
		timer.Stop()
		stallCancel()
		_ = body.Close()

		offset += n

		if copyErr == nil {
			break
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if attempt >= maxInJobRetries {
			return fmt.Errorf("transfer failed after %d retries at offset %d: %w", attempt, offset, copyErr)
		}

		log.Warn("ingest: transfer interrupted, retrying with Range",
			"offset", offset, "attempt", attempt, "err", copyErr)
	}

	expectedLen, err := w.MirakcClient.HeadRecordStream(ctx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}

	// pid_type_changes > 0 は録画中に PMT が PID を付け替えたということ。
	// 種別は最後に見たものを採用するので、変化そのものはここにしか残らない
	// （docs/recording.md §1「例外の境界」）。
	log.Info("ingest: transfer complete", "bytes", offset,
		"drops", counter.TotalDrops(), "errors", counter.TotalErrors(),
		"scrambled", counter.TotalScrambled(),
		"pid_type_changes", counter.TypeChanges())

	metrics.IngestBytes.Add(float64(offset))
	metrics.IngestDroppedPackets.Add(float64(counter.TotalDrops()))
	metrics.IngestErrorPackets.Add(float64(counter.TotalErrors()))
	metrics.IngestScrambledPackets.Add(float64(counter.TotalScrambled()))

	if err := w.commit(ctx, recordingID, relPath, offset, counter); err != nil {
		return fmt.Errorf("committing ingest: %w", err)
	}

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	result = "success"

	// encode 投入はヒント。desired（encode_profiles）− observed（encoded assets）
	// を埋めるレベルトリガー（命令的チェーンではない。issue #65）。
	enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID)

	// thumbnail 投入はヒント。desired − observed を EnqueueThumbnailIfNeeded が
	// 判定する（レベルトリガー。命令的チェーンではない。issue #66）。
	// River クライアントが無いテスト経路では黙ってスキップする。
	if riverClient, clientErr := river.ClientFromContextSafely[pgx5.Tx](ctx); clientErr == nil {
		if enqueueErr := EnqueueThumbnailIfNeeded(ctx, w.Pool, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue thumbnail job", "recording_id", recordingID, "err", enqueueErr)
		}
	}

	if _, err := w.MirakcClient.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (committed OK)", "err", err)
	}

	return nil
}

// connectRetryDelay は StreamRecord への接続リトライの待ち時間を指数バックオフで返す。
// attempt は 0 始まり。connectRetryMaxDelay で頭打ちにする。
func connectRetryDelay(attempt int) time.Duration {
	delay := connectRetryBaseDelay << attempt
	if delay > connectRetryMaxDelay || delay <= 0 {
		return connectRetryMaxDelay
	}
	return delay
}

// hasOriginalMediaAsset は recordingID に対する kind='original' の media_asset
// 行が既に存在するかを返す。ingest の冪等性チェックに使う（Work 参照）。
func (w *IngestWorker) hasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	q := sqlcgen.New(w.Pool)
	_, err := q.GetOriginalMediaAssetID(ctx, recordingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("querying media_assets: %w", err)
}

func (w *IngestWorker) lookupRecordingID(ctx context.Context, args IngestJobArgs) (int64, error) {
	q := sqlcgen.New(w.Pool)
	recID, err := q.GetRecordSyncRecordingID(ctx, sqlcgen.GetRecordSyncRecordingIDParams{
		Site:     args.Site,
		RecordID: args.RecordID,
	})
	if err != nil {
		return 0, fmt.Errorf("querying record_sync: %w", err)
	}
	if recID == nil {
		return 0, fmt.Errorf("record_sync (%s, %s) has no recording_id", args.Site, args.RecordID)
	}
	return *recID, nil
}

// determineRelPath は保存先の相対パスと、それを解決した絶対パスを返す。
// relPath は mirakc の contentPath 由来なので、メディアディレクトリの外を
// 指していないことを検証する。
//
// 返す relPath には `sites/{site}/` を前置する（issue #186 M4-14）。アーカイブ
// （media_assets）は site 列を持たず単一だが、contentPath は mirakc インスタンス
// スコープの名前なので、2 サイトが同じ contentPath で録ると同じ実ファイルを
// 取り合う（DB は一意索引で片方の commit を落とすが、実ファイルは先に書いた方が
// 上書きされて壊れる）。ingest は原本 rel_path の唯一の書き手なので、ここで
// 前置すれば入力（contentPath の有無や形）に関わらず名前空間が保たれる —
// reconciler の contentPath 生成（mirakc 側の録画バッファ）や運用者の
// filename_template（ユーザーが書ける）に前置を委ねると、Rokuban 以外が作った
// record や旧いテンプレートの record で名前空間が破れる。
//
// **前置の 1 段目は固定の `sites/` で、その下に site 名を置く。** 当初案の
// 「site 名をそのまま先頭成分にする」（`{site}/...`）は、既存行（前置前に
// ingest 済みの rel_path）の先頭成分と site 名が偶然一致した場合に衝突する
// --- 例えば filename_template が `"tokyo/..."` のような静的接頭辞を書いていて、
// かつ site 名が `tokyo` だと、新規 ingest の `tokyo/shared/rec.m2ts` が既存行と
// 同じ rel_path になり、DB の一意索引が通る前に実ファイルが上書きされる
// （PR #196 のレビューで実測。site 名の構文 `^[a-z0-9]([_-]?[a-z0-9])*$` は
// 日付ディレクトリ名や静的な語（`anime` 等）も許すため、理論上だけの懸念では
// ない）。`sites/` を固定の 1 段目に挟むことで、新規 ingest の rel_path は
// 常に `sites/` から始まり、それ以前の任意の contentPath / filename_template
// が `sites/` から始まっていない限り構造的に衝突しない（`catalog/` /
// `thumbnails/` と同じ「トップレベル予約ディレクトリ」の追加であり、
// この 3 つのいずれから始まる既存行が無いことが前提。詳細は docs/storage.md
// §5「rel_path の名前空間」）。
//
// 前置に使うのは args.Site（w.Site ではない）。Work は determineRelPath を呼ぶ
// 前に verifySite（internal/worker/worker.go）で args.Site が w.Site（空なら
// db.DefaultSite）と一致することを検査済みなので値としては同じだが、
// verifySite は w.Site="" のときに args.Site="" を通さない（正規化後の
// "default" と比較して弾く）ため、verifySite を通過した args.Site は常に
// 非空・正規化済みである。w.Site を使うと単体テストの部分構成
// （w.Site 未設定）で同じ正規化をここで再実装する必要がある。
//
// contentPath / Content.Path がどちらも空だと relPath が "."（カレント
// ディレクトリ）になる。前置前はこの relPath="." がそのまま mediapath.Resolve
// に渡り "path escapes the media directory" で明示的に弾かれていた。前置後は
// "sites/{site}/." が Join/Clean で "." が消えて "sites/{site}" という一見
// 正当なパスになり Resolve を通ってしまう。すると os.Create が
// "{media_dir}/sites/{site}" を*通常ファイル*として作ってしまい、以後その
// site 配下に別の contentPath を書こうとする ingest が全て MkdirAll で
// "not a directory" になる（前置によって初めて顕在化する壊れ方。前置前は
// Resolve の明示的なエラーで止まっていたので発生しなかった）。前置前に弾く
// （下記）。
//
// `sites/{site}/` は site 名の構文制約（internal/config.validateSiteName、
// issue #183 M4-11）に依存する安全前提の上に成立している --- site 名に "/" が
// 含まれないことを保証しているので単純な文字列結合で足りる。
//
// 前置前に ingest 済みの既存行は移行しない（マイグレーションを書かない）。
// 新規 ingest 分だけ `sites/{site}/` が付き、ディスク上は前置あり/なしが
// 混在する。rel_path をパースする読者はいない（rescueAssetKind は拡張子しか
// 見ない）ので混在は無害 --- ただし上記の通り、混在する既存行が `sites/` から
// 始まっていないことが前提。
func (w *IngestWorker) determineRelPath(ctx context.Context, args IngestJobArgs) (relPath, fullPath string, err error) {
	record, err := w.MirakcClient.GetRecord(ctx, args.RecordID)
	if err != nil {
		return "", "", fmt.Errorf("getting mirakc record: %w", err)
	}
	if cp := record.Recording.Options.ContentPath; cp != nil && *cp != "" {
		relPath = *cp
	} else {
		relPath = filepath.Base(record.Content.Path)
	}
	// contentPath / Content.Path がどちらも空だと filepath.Base("") == "."。
	// 前置すると mediapath.Resolve の脱出検知をすり抜けてディレクトリが作られて
	// しまう（上の doc コメント参照）ので、前置前に明示的に弾く。
	if relPath == "." || relPath == "/" {
		return "", "", fmt.Errorf("mirakc record %s has no usable content path (contentPath and Content.Path both empty)", args.RecordID)
	}
	// rel_path のパス区切りは DB 上で '/' 規約（internal/worker/encode.go の
	// EncodedRelPath 参照）。args.Site は site 名の構文制約で '/' を含み得ない
	// ので単純な文字列結合で足りる。
	relPath = "sites/" + args.Site + "/" + relPath
	fullPath, err = mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return "", "", err
	}
	return relPath, fullPath, nil
}

func (w *IngestWorker) commit(ctx context.Context, recordingID int64, relPath string, size int64, counter *tsstat.Counter) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		return fmt.Errorf("inserting media_asset: %w", err)
	}

	// 原本のコミットと同じ tx で「この録画の望ましい最終状態」を焼く
	// （issue #103。resolveAndSnapshotEncodePolicy の doc コメント参照）。
	if err := w.resolveAndSnapshotEncodePolicy(ctx, q, recordingID); err != nil {
		return fmt.Errorf("snapshotting encode policy: %w", err)
	}

	stats := counter.Stats()
	pids := make([]int, 0, len(stats))
	for pid := range stats {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		s := stats[pid]
		// 分類できなかった PID は種別なし（NULL）。空文字を「未分類」という値として
		// 永続化しない（M2-13, issue #24）。
		var pidType *string
		if s.Type != "" {
			t := s.Type
			pidType = &t
		}
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          int32(pid),
			Packets:      s.Packets,
			Drops:        s.Drops,
			Errors:       s.Errors,
			Scrambled:    s.Scrambled,
			PidType:      pidType,
		}); err != nil {
			return fmt.Errorf("inserting drop_stat for PID %d: %w", pid, err)
		}
	}

	if counter.TotalScrambled() > 0 {
		event := db.QualityEvent{
			At:    time.Now(),
			Event: "bcas_anomaly",
		}
		evJSON, err := json.Marshal([]db.QualityEvent{event})
		if err != nil {
			return fmt.Errorf("marshalling quality event: %w", err)
		}
		if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{
			Events: evJSON,
			ID:     recordingID,
		}); err != nil {
			return fmt.Errorf("appending quality event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// resolveAndSnapshotEncodePolicy は「この録画の望ましい最終状態」を凍結する
// （issue #103）。issue #159 で recordings.keep_original / encode_profiles から
// 衛星表 recording_encode_policy に切り出されたため、凍結 = この関数が呼ぶ
// FreezeRecordingEncodePolicy による当該表への行 INSERT である（不変条件 3
// 「コミット = DB 行」・不変条件 10「意味を持たない行を作らない」: 行が無い =
// 未凍結、行がある = 凍結済み）。呼び出し元の commit が原本 media_asset の
// INSERT と同じ tx で呼ぶ。
//
// # 凍結か、毎パス再導出（reservations 経由の参照）か
//
// 再導出は選べない。導出元（reservations / program_overrides / program_intents）は
// 放送終了 + 猶予後に GC される寿命の短い表だが、recordings（と衛星表
// recording_encode_policy）は永続資産（CLAUDE.md 不変条件 12「表は行の寿命で
// 割る」）。導出に依存させると、番組が EPG から消えて GC された時点で desired が
// 空になり、エンコード未完了の録画で原本削除が止まる／再エンコードが投入
// できなくなる。導出元が先に死ぬ表の desired を、値のコピーではなく「参照」で
// 持つことはできない。
//
// 凍結した値は「この録画の望ましい最終状態」であり、recording_encode_policy 行は
// recordings 行と同時に生まれて同時に死ぬ（PRIMARY KEY = recordings への FK）ので
// 不変条件 12 には反しない（ruler の 1 パスの出力である reservations.base とは
// 寿命が別）。ただし凍結する以上、凍結より後の override 変更はこの録画には
// 反映されないという境界が生まれる。
//
// # 凍結する瞬間
//
// recordings 行自体は watcher が録画開始時に作るが、この関数はそこでは呼ばれない。
// docs/recording/reservation-model.md §4.5 は「encodeProfiles / keepOriginal は
// 録画開始後の変更でも効く」と約束しており、これを満たせるのは ingest が原本を
// コミットする瞬間だけ（録画は放送終了後に確定し、ingest はその直後に走る）。
// 呼び出し元 commit は encode ジョブの投入（EnqueueMissingEncodes）より必ず先に
// この関数を呼ぶ — 順序が逆だと初回パスで desired が空のまま enqueue され、
// 実際に投入されるのは次の ingest 再試行（起きるとは限らない）まで遅延する。
//
// # 予約をどのキーで引くか（issue #149）
//
// recordings.reservation_id（bigint FK、ON DELETE SET NULL。issue #158 で
// 列自体を削除済み）は宛先にしない。ruler は EPG フリッカー・ルール編集・dedup
// で予約を導出削除・再実体化し、
// そのたびに reservations.id が変わる（#53 / #98 / #99 が繰り返し踏んでいる
// 族。CLAUDE.md 不変条件 9「identity」の 5 例目）。録画開始から ingest 完了
// までの窓（番組の尺ぶん、数時間）でこれが 1 回でも起きると FK が NULL に
// 落ち、「予約が無い」と誤認して encode policy を凍結し損なう —— ログにも
// 出ないので気付かれない。
//
// 代わりに放送イベントキー (site, network_id, service_id, event_id) で引く。
// recordings はこの 4 列を録画開始時から凍結して持つ（導出器が作るキーでは
// ない）ので、予約の再実体化を跨いでも変わらない。GetReservationEncodePolicyByEvent
// は program_snapshots で (network_id, service_id, event_id) → program_id を
// 引いてから reservations を program_id で結合する
// （internal/db/queries/recording_policy.sql）。
//
// program_snapshots は放送後 GC される寿命の短い表（docs/storage.md §6）だが、
// ingest は録画終了直後（GC の猶予期間より十分前）に走るので、この GC 前提は
// 通常経路では効かない。
//
// recordings.source（internal/db/recording_source.go の DeriveRecordingSource）
// は「引けなかった」の異常度を判定する軸として使えない: 'rule' は「作成時点で
// 予約があり、かつ program_intents.action='record' の行が無かった」を意味する
// ので、'rule' で JOIN が失敗するのは常に異常系（GC が想定より早く走った、
// または予約が恒久的に削除された）。しかし 'manual' は「intent が
// action='record' だった（予約の有無に関わらず）」と「そもそも予約が最初から
// 無かった（手動で mirakc に起こされた録画等、日常的）」の 2 つの独立した
// 経路を 1 つの値に潰している（同ファイルのコメント参照）ため、'manual' の
// JOIN 失敗が異常かどうかはこの列だけからは判別できない —— 前者（ユーザーが
// 手動予約して encodeProfiles を指定した録画）で解決に失敗すると、まさに
// issue #149 が問題にした「静かにエンコードされない」が残る。区別できない
// 以上、どちらの source でも黙って return せず識別子
// （site/network_id/service_id/event_id）と recordingID をログに残す。
// 'rule' は slog.Warn（常に異常）、'manual' は slog.Info（日常的なケースと
// 異常なケースが混在するため騒がしくしない）に分ける。
//
// # 解決に失敗しても凍結する（issue #159）
//
// GetReservationEncodePolicyByEvent が pgx.ErrNoRows を返した場合でも、
// FreezeRecordingEncodePolicy は既定値（keepOriginal='always',
// encodeProfiles=[]）で必ず呼ぶ --- 凍結そのものをスキップしない。旧実装
// （recordings.keep_original / encode_profiles が列だった頃）はここで書かずに
// return していたが、それでも列は CREATE TABLE の既定値のまま残っていたので
// 実質的には「常に凍結されている」のと同じだった。衛星表 recording_encode_policy
// は行が無ければ既定値をどこにも持たないので、ここで書かないと 2 つの契約が
// 破れる: (1) migration 00032 の backfill は「原本 media_asset を持つ録画は
// 凍結済み」を基準にしており、ingest 完了後もこの基準を保つ必要がある、
// (2) issue #133 の事後追加（AppendRecordingEncodeProfiles）は「行が既にある」
// ことを前提に書けるようになった（doc コメント冒頭参照）ので、予約が無い
// 録画（手動で mirakc に起こされた録画等、日常的なケース）にも行が無ければ
// 追加できなくなってしまう。
//
// # 冪等性
//
// ingest は再試行される。この関数は毎回同じ入力から同じ値を書くだけの INSERT
// で、既存の encoded media_assets の有無を見て分岐しない —— 既に一部の
// エンコードが完了しているからといって desired を空に戻すような分岐は作らない
// （issue #103 の「罠」）。FreezeRecordingEncodePolicy は ON CONFLICT を持たない
// 素の INSERT だが、これは「1 度しか呼ばれない」という別の不変（Work が転送
// 開始前に GetOriginalMediaAssetID で冪等性チェックするため、この tx 自体が
// 録画ごとに 1 回しか実行されない）に依っている。CreateMediaAsset（原本の
// INSERT）と同じ前提を共有する。
//
// # EncodeProfiles の nil
//
// db.ReservationOptions.EncodeProfiles は *[]string で nil=未指定 /
// &[]string{}=「エンコードなし」という明示的な override を区別する
// （internal/db/models.go の ReservationOptions のコメント）。しかし
// recording_encode_policy.encode_profiles は NOT NULL text[] で「未指定」という
// 第三の状態を表現できないので、ここでは両者を等しく '{}' に潰すと決める。
// 区別が必要な場面（override の差分表示等）は program_overrides.overrides
// 自身に当たればよく、このスナップショットの役目ではない。
//
// # keepOriginal='until_encoded' × encodeProfiles=[] のクランプ（issue #104 との整合）
//
// rules.keep_original / rules.encode_profiles にはこの組み合わせを禁止する CHECK が
// あるが、実効値はルール単独では決まらない --- override が `keepOriginal:
// until_encoded` だけを立て、ルール側の `encodeProfiles` が空（またはルール自体が
// `keep_original='always'`）というドリフトが EffectiveOptions のマージ結果として
// 生成されうる。ルールと override はそれぞれ自分の表の中では整合していても、
// マージ結果の整合は誰も検査していない。
//
// recording_encode_policy にも同じ組み合わせを禁止する CHECK（issue #104、
// `until_encoded` は encode_profiles が非空であることを要求する。issue #159 で
// 00020 から移設）があり、そのまま実効値を書くとこの tx が CHECK 違反で
// ロールバックする --- このメソッドは原本 media_asset の INSERT と同一 tx で
// 呼ばれるため、ロールバックは「録画そのものが消失する」に直結する（不変条件 3
// 「コミット = DB 行」）。原本を失うリスクを負ってまで守る価値のある不変では
// ないので、書く直前に安全側へ倒す: 実効的な encode_profiles が空で
// keepOriginal が 'until_encoded' なら、'always' に倒してから書く。ユーザーの
// 意図（override の値そのもの）は program_overrides 側に残るので失われない ---
// 失われるのはこの録画のスナップショットにおける効力だけで、次にルールが
// プロファイルを持てば別の録画では正しく until_encoded になる。
func (w *IngestWorker) resolveAndSnapshotEncodePolicy(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	rec, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading recording %d: %w", recordingID, err)
	}

	row, err := q.GetReservationEncodePolicyByEvent(ctx, sqlcgen.GetReservationEncodePolicyByEventParams{
		Site:      rec.Site,
		NetworkID: rec.NetworkID,
		ServiceID: rec.ServiceID,
		EventID:   rec.EventID,
	})
	// 解決に失敗しても凍結自体はスキップしない（issue #159。doc コメント
	// 「解決失敗時も凍結する」参照）。既定値（'always' / '{}'）で凍結する ---
	// 何も INSERT しないと、原本 media_asset の有無で「凍結済みか」を判定する
	// 不変条件（migration 00032 の backfill と同じ基準）が破れ、かつ issue #133
	// の事後追加（AppendRecordingEncodeProfiles）が「行が既にある」ことを前提に
	// できなくなる。
	keepOriginal := "always"
	encodeProfiles := []string{}
	if err != nil {
		if !errors.Is(err, pgx5.ErrNoRows) {
			return fmt.Errorf("loading reservation encode policy for recording %d (site=%s network_id=%d service_id=%d event_id=%d): %w",
				recordingID, rec.Site, rec.NetworkID, rec.ServiceID, rec.EventID, err)
		}
		// source='rule' は常に異常系（doc コメント「予約をどのキーで引くか」
		// 参照）なので Warn、source='manual' は日常的なケース（予約が最初から
		// 無い）と異常なケース（intent action='record' だったが予約が
		// 恒久的に削除された）が混在するので Info に落とす。どちらの source
		// でも黙って return しない —— 判別できないことをログの欠落で
		// 埋め合わせない。
		logArgs := []any{
			"recording_id", recordingID,
			"site", rec.Site,
			"network_id", rec.NetworkID,
			"service_id", rec.ServiceID,
			"event_id", rec.EventID,
		}
		if rec.Source == db.SourceRule {
			slog.Warn("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		} else {
			slog.Info("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		}
	} else {
		eff, err := db.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
		if err != nil {
			return fmt.Errorf("computing effective options for reservation %d: %w", row.Reservation.ID, err)
		}
		if eff.KeepOriginal != nil {
			keepOriginal = *eff.KeepOriginal
		}
		if eff.EncodeProfiles != nil {
			encodeProfiles = *eff.EncodeProfiles
		}
	}

	// クランプ（doc コメント「keepOriginal='until_encoded' × encodeProfiles=[] の
	// クランプ」参照）。ルール単独・override 単独ではそれぞれ禁則を満たしていても、
	// マージ結果としてこの組み合わせが生成されうる。cardinality(encode_profiles) > 0
	// を要求する CHECK（issue #104）とここで矛盾すると、このメソッドを呼ぶ tx
	// （原本 media_asset の INSERT と同一）ごとロールバックし録画が消える
	// （不変条件 3）ため、書く前に安全側へ倒す。
	if keepOriginal == "until_encoded" && len(encodeProfiles) == 0 {
		keepOriginal = "always"
	}

	return q.FreezeRecordingEncodePolicy(ctx, sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    recordingID,
		KeepOriginal:   keepOriginal,
		EncodeProfiles: encodeProfiles,
	})
}

type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}
