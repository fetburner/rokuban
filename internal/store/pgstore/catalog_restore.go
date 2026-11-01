package pgstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Restore は catalog の全表を 1 tx で upsert し、分類ルールを再評価する。
// この tx は分類ルールの再評価と直列化する。直列化は最初の書き込みより前から始まる。
// 呼び出し側は media asset のファイルロックを先に取る。
//
// 順序: 再評価ロック → rules（+ 子）→ label_rules → program_snapshots →
// program_intents / program_overrides → recordings → recording_purge_requests →
// recording_watched → recording_encode_policy → media_assets → チャプター所有 2 表 →
// drop_stats → drop_positions → media_asset_ts_scans → 分類ルールの全件再評価 →
// ID シーケンスのリセット → ruler 入力の版の更新。
// 全部 1 トランザクションで、途中失敗なら何も残さない。
func (s *Catalog) Restore(ctx context.Context, doc *catalogdoc.Document, options store.RestoreOptions) (*store.RestoreResult, error) {
	if doc == nil {
		return nil, fmt.Errorf("catalog document is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning catalog restore: %w", err)
	}
	defer rollback(tx)

	result, err := applyDocument(ctx, tx, doc, options, s.siteNames, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("restoring catalog: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing catalog restore: %w", mapDatabaseError(err))
	}
	return result, nil
}

// snapshotKey は program_snapshots の主キー。
type snapshotKey struct {
	site      string
	programID int64
}

// insertableSnapshot は snapshot 行が DB の制約を通るかを返す。
//
// **見るのは DB が実際に拒否するものだけ**（`program_snapshots` に掛かっている
// CHECK は program_snapshots_channel_type_check の
// `channel_type IN ('GR','BS','CS','SKY')` 1 つだけ。他の列は
// NOT NULL があるだけで、非空も正数も要求していない）。
//
// **「放送を同定できないから」で落としてはならない。** 行を落とすと FK 先を
// 失う program_intents / program_overrides も連動して落ちる —— それは
// 「この番組を録れ / 録るな」というユーザーが明示した意図であって、導出できない。
// 空のサービス名（SDT が名前を持たない構成は実在し、`epg_services.name` にも
// 非空制約は無い）程度で意図を捨てるのは釣り合わない。ここの責務は
// 「1 行で INSERT が落ちてトランザクションごと巻き戻るのを防ぐ」ことに限る。
func insertableSnapshot(s catalogdoc.ProgramSnapshot) bool {
	switch s.ChannelType {
	case "GR", "BS", "CS", "SKY":
		return true
	default:
		return false
	}
}

// applyDocument は 1 つのトランザクション内で catalogdoc.Document の各表を FK 順に復元する。
func applyDocument(ctx context.Context, tx pgx.Tx, doc *catalogdoc.Document, options store.RestoreOptions, siteNames []string, storeName string) (*store.RestoreResult, error) {
	q := sqlcgen.New(tx)
	res := &store.RestoreResult{}

	// worker のジョブと同じロックで直列化する（古いスナップショットで評価した
	// ジョブが、この結果を後から上書きしないように）。**最初の書き込みより前**に取る。
	// 途中で取ると、recordings の upsert のトリガーが label_rule_hits の行ロックを
	// 持ったまま advisory lock を待ち、ジョブ（advisory lock 保持 → その行を待つ）と
	// デッドロックして、rescue が犠牲になれば 1 世代の復元が丸ごと失敗する。
	if err := q.LockLabelRuleReevaluation(ctx); err != nil {
		return nil, fmt.Errorf("locking label rule re-evaluation: %w", err)
	}
	if err := applyRules(ctx, q, doc.Rules, res); err != nil {
		return nil, err
	}
	// 分類ルールは recordings より前に書く。recordings の INSERT トリガーが
	// その時点のルール集合で当たりを評価するので、後に書くと新規行の当たりが
	// 全件再評価（下）まで空になる。
	if err := applyLabelRules(ctx, q, doc.LabelRules, res); err != nil {
		return nil, err
	}

	// program_intents / program_overrides は program_snapshots を参照する。
	// 欠損キーは後段の helper に渡して、参照行も連動して復元しない。
	skippedSnapshots, err := applyProgramSnapshots(ctx, q, doc.ProgramSnapshots, res)
	if err != nil {
		return nil, err
	}

	if err := applyProgramIntents(ctx, q, doc.ProgramIntents, skippedSnapshots, res); err != nil {
		return nil, err
	}
	if err := applyProgramOverrides(ctx, q, doc.ProgramOverrides, skippedSnapshots, res); err != nil {
		return nil, err
	}

	if err := applyRecordings(ctx, q, doc.Recordings, res); err != nil {
		return nil, err
	}

	// recording_purge_requests は recordings への FK を持つので recordings の
	// upsert より後に書く。doc.RecordingPurgeRequests に載っていない録画には
	// 何も書かない --- 「即時削除の要求は無い」は行の不在そのものが意味を持つ
	// （不変条件 10）。
	if err := applyRecordingPurgeRequests(ctx, q, doc.RecordingPurgeRequests, res); err != nil {
		return nil, err
	}
	if err := applyRecordingWatched(ctx, q, doc.RecordingWatched, res); err != nil {
		return nil, err
	}

	// recording_encode_policy は recordings への FK を持つので recordings の
	// upsert より後に書く。doc.RecordingEncodePolicies に載っていない録画には
	// 何も書かない --- 「未凍結」は行の不在そのものが意味を持つ（不変条件 10）。
	if err := applyRecordingEncodePolicies(ctx, q, doc.RecordingEncodePolicies, res); err != nil {
		return nil, err
	}

	if err := applyMediaAssets(ctx, tx, q, doc.MediaAssets, res, options.MissingAssetCopyKeys, storeName); err != nil {
		return nil, err
	}

	// チャプターの所有 2 表は media_assets の後（区間が所有の行を FK で指すので
	// この順）。doc.RecordingChapterOwnerships に載っていない録画には何も書かない
	// --- 「自動層のまま」は行の不在そのものが意味を持つ（不変条件 10）。
	// ユーザーの手作業なので、落とすと取り返しがつかない。
	if err := applyRecordingChapterOwnerships(ctx, q, doc.RecordingChapterOwnerships, res); err != nil {
		return nil, err
	}
	if err := applyRecordingChapterSpans(ctx, q, doc.RecordingChapterSpans, res); err != nil {
		return nil, err
	}

	if err := applyDropStats(ctx, q, doc.DropStats, res); err != nil {
		return nil, err
	}
	if err := applyDropPositions(ctx, q, doc.DropPositions, res); err != nil {
		return nil, err
	}

	// media_asset_ts_scans は media_assets を参照し、文書化された復元順で
	// drop_positions の後に来る。旧文書にはキーが無く、原本が計測済みだったと
	// 言えるのは drop_stats の行だけなので、そこから補う。
	tsScans := doc.MediaAssetTSScans
	if doc.MediaAssetTSScans == nil {
		tsScans = legacyMediaAssetTSScans(doc)
	}
	if err := applyMediaAssetTSScans(ctx, q, tsScans, res); err != nil {
		return nil, err
	}

	// label_rule_hits は catalog に入れない導出値なので、ここで全件を再評価して
	// 閉じる。トリガーは title が変わった行と新規行しか見ないので、DB に残っていた
	// 既存録画の当たりは、復元したルール集合に対して古いままになりうる。
	// worker の定期再評価（15 分）に任せず同じ tx で閉じる。
	if _, err := reevaluateLabelRulesTx(ctx, tx); err != nil {
		return nil, fmt.Errorf("re-evaluating label rules after rescue: %w", err)
	}

	if err := q.CatalogResetRulesIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting rules id seq: %w", err)
	}
	if err := q.CatalogResetLabelRulesIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting label_rules id seq: %w", err)
	}
	if err := q.CatalogResetRecordingsIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting recordings id seq: %w", err)
	}
	if err := q.CatalogResetMediaAssetsIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting media_assets id seq: %w", err)
	}
	// 版の更新は最後に置く。ruler の書き込み tx の版照合は版の行ロックを取るので、
	// 先頭に置くと Restore の tx が終わるまで ruler のコミットが待たされる。
	if err := bumpRulerInputRevisions(ctx, q, siteNames); err != nil {
		return nil, fmt.Errorf("bumping ruler input revisions after catalog restore: %w", err)
	}
	return res, nil
}

// applyLabelRules は label_rules を id 保持の upsert で復元し、件数を
// res.LabelRules に書く。
func applyLabelRules(ctx context.Context, q *sqlcgen.Queries, rules []catalogdoc.LabelRule, res *store.RestoreResult) error {
	for _, r := range rules {
		if err := q.CatalogUpsertLabelRule(ctx, sqlcgen.CatalogUpsertLabelRuleParams{
			ID: r.ID, Key: r.Key, Value: r.Value, Keyword: r.Keyword,
			Priority: r.Priority, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("upserting label_rule %d: %w", r.ID, err)
		}
	}
	res.LabelRules = len(rules)
	return nil
}

// applyRules は rules とその従属表を復元し、件数を res.Rules に書く。
func applyRules(ctx context.Context, q *sqlcgen.Queries, rules []catalogdoc.Rule, res *store.RestoreResult) error {
	for _, rule := range rules {
		if err := upsertRule(ctx, q, rule); err != nil {
			return fmt.Errorf("upserting rule %d: %w", rule.ID, err)
		}
	}
	res.Rules = len(rules)
	return nil
}

// applyProgramSnapshots は program_snapshots を復元し、件数を res.ProgramSnapshots /
// res.SkippedProgramSnapshots に書いて、復元できなかったキーを返す。
func applyProgramSnapshots(ctx context.Context, q *sqlcgen.Queries, snapshots []catalogdoc.ProgramSnapshot, res *store.RestoreResult) (map[snapshotKey]struct{}, error) {
	skipped := make(map[snapshotKey]struct{})
	count := 0
	skippedCount := 0
	for _, s := range snapshots {
		// 壊れた 1 行で災害復旧そのものを止めない。program_snapshots の
		// channel_type には CHECK があるため、拒否される行をそのまま INSERT すると
		// トランザクション全体がロールバックする（TestRescueFile_MalformedSnapshotIsSkippedAndAssetsSurvive）。
		// 放送と retention_grace で GC される導出データなので、行を落として永続資産の
		// 復元を続ける。
		if !insertableSnapshot(s) {
			slog.Warn("rescue: skipping program_snapshot the database would reject",
				"site", s.Site, "program_id", s.ProgramID, "channel_type", s.ChannelType)
			skipped[snapshotKey{site: s.Site, programID: s.ProgramID}] = struct{}{}
			skippedCount++
			continue
		}
		if err := q.CatalogUpsertProgramSnapshot(ctx, sqlcgen.CatalogUpsertProgramSnapshotParams{
			Site:        s.Site,
			ProgramID:   s.ProgramID,
			Title:       s.Title,
			StartAt:     s.StartAt,
			DurationMs:  s.DurationMs,
			NetworkID:   s.NetworkID,
			ServiceID:   s.ServiceID,
			ChannelType: s.ChannelType,
			Channel:     s.Channel,
			EventID:     s.EventID,
			ServiceName: s.ServiceName,
			UpdatedAt:   s.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting program_snapshot %s/%d: %w", s.Site, s.ProgramID, err)
		}
		count++
	}
	res.ProgramSnapshots = count
	res.SkippedProgramSnapshots = skippedCount
	return skipped, nil
}

// applyProgramIntents は program_snapshots を参照する program_intents を復元し、
// 件数を res.ProgramIntents / res.SkippedProgramIntents に書く。
func applyProgramIntents(ctx context.Context, q *sqlcgen.Queries, intents []catalogdoc.ProgramIntent, skippedSnapshots map[snapshotKey]struct{}, res *store.RestoreResult) error {
	count := 0
	skipped := 0
	for _, i := range intents {
		if _, missing := skippedSnapshots[snapshotKey{site: i.Site, programID: i.ProgramID}]; missing {
			skipped++
			continue
		}
		if err := q.CatalogUpsertProgramIntent(ctx, sqlcgen.CatalogUpsertProgramIntentParams{
			Site:      i.Site,
			ProgramID: i.ProgramID,
			Action:    i.Action,
			CreatedAt: i.CreatedAt,
			UpdatedAt: i.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("upserting program_intent %s/%d: %w", i.Site, i.ProgramID, err)
		}
		count++
	}
	res.ProgramIntents = count
	res.SkippedProgramIntents = skipped
	return nil
}

// applyProgramOverrides は program_snapshots を参照する program_overrides を復元し、
// 件数を res.ProgramOverrides / res.SkippedProgramOverrides に書く。
func applyProgramOverrides(ctx context.Context, q *sqlcgen.Queries, overrides []catalogdoc.ProgramOverride, skippedSnapshots map[snapshotKey]struct{}, res *store.RestoreResult) error {
	count := 0
	skipped := 0
	for _, o := range overrides {
		if _, missing := skippedSnapshots[snapshotKey{site: o.Site, programID: o.ProgramID}]; missing {
			skipped++
			continue
		}
		if err := q.CatalogUpsertProgramOverride(ctx, sqlcgen.CatalogUpsertProgramOverrideParams{
			Site:      o.Site,
			ProgramID: o.ProgramID,
			Overrides: o.Overrides,
			CreatedAt: o.CreatedAt,
			UpdatedAt: o.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("upserting program_override %s/%d: %w", o.Site, o.ProgramID, err)
		}
		count++
	}
	res.ProgramOverrides = count
	res.SkippedProgramOverrides = skipped
	return nil
}

// applyRecordings は recordings を復元し、件数を res.Recordings に書く。
func applyRecordings(ctx context.Context, q *sqlcgen.Queries, recordings []catalogdoc.Recording, res *store.RestoreResult) error {
	for _, r := range recordings {
		qe := r.QualityEvents
		if len(qe) == 0 || bytes.Equal(bytes.TrimSpace(qe), []byte("null")) {
			qe = []byte("[]")
		}
		if err := q.CatalogUpsertRecording(ctx, sqlcgen.CatalogUpsertRecordingParams{
			ID:                r.ID,
			RuleID:            r.RuleID,
			Source:            r.Source,
			Site:              r.Site,
			NetworkID:         r.NetworkID,
			ServiceID:         r.ServiceID,
			EventID:           r.EventID,
			ServiceName:       r.ServiceName,
			ChannelType:       r.ChannelType,
			Channel:           r.Channel,
			Title:             r.Title,
			Description:       r.Description,
			Extended:          r.Extended,
			Genres:            r.Genres,
			IsFree:            r.IsFree,
			ProgramStartAt:    r.ProgramStartAt,
			ProgramDurationMs: r.ProgramDurationMs,
			Status:            r.Status,
			StartedAt:         r.StartedAt,
			EndedAt:           r.EndedAt,
			QualityEvents:     qe,
			DeletedAt:         r.DeletedAt,
			SupersededAt:      r.SupersededAt,
			PurgedAt:          r.PurgedAt,
			CreatedAt:         r.CreatedAt,
			UpdatedAt:         r.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("upserting recording %d: %w", r.ID, err)
		}
	}
	res.Recordings = len(recordings)
	return nil
}

// applyRecordingPurgeRequests は recording_purge_requests を復元し、件数を
// res.RecordingPurgeRequests に書く。
func applyRecordingPurgeRequests(ctx context.Context, q *sqlcgen.Queries, requests []catalogdoc.RecordingPurgeRequest, res *store.RestoreResult) error {
	for _, p := range requests {
		if err := q.CatalogUpsertRecordingPurgeRequest(ctx, sqlcgen.CatalogUpsertRecordingPurgeRequestParams{
			RecordingID: p.RecordingID,
			RequestedAt: p.RequestedAt,
		}); err != nil {
			return fmt.Errorf("upserting recording_purge_request %d: %w", p.RecordingID, err)
		}
	}
	res.RecordingPurgeRequests = len(requests)
	return nil
}

func applyRecordingWatched(ctx context.Context, q *sqlcgen.Queries, rows []catalogdoc.RecordingWatched, res *store.RestoreResult) error {
	for _, row := range rows {
		if err := q.CatalogUpsertRecordingWatched(ctx, sqlcgen.CatalogUpsertRecordingWatchedParams{
			RecordingID: row.RecordingID,
			WatchedAt:   row.WatchedAt,
		}); err != nil {
			return fmt.Errorf("upserting recording_watched %d: %w", row.RecordingID, err)
		}
	}
	res.RecordingWatched = len(rows)
	return nil
}

// applyRecordingEncodePolicies は recording_encode_policy を復元し、件数を
// res.RecordingEncodePolicies に書く。
func applyRecordingEncodePolicies(ctx context.Context, q *sqlcgen.Queries, policies []catalogdoc.RecordingEncodePolicy, res *store.RestoreResult) error {
	for _, p := range policies {
		profiles := p.EncodeProfiles
		if profiles == nil {
			profiles = []string{}
		}
		if err := q.CatalogUpsertRecordingEncodePolicy(ctx, sqlcgen.CatalogUpsertRecordingEncodePolicyParams{
			RecordingID:    p.RecordingID,
			KeepOriginal:   p.KeepOriginal,
			EncodeProfiles: profiles,
			CreatedAt:      p.CreatedAt,
			UpdatedAt:      p.UpdatedAt,
			CmDetect:       p.CMDetect,
		}); err != nil {
			return fmt.Errorf("upserting recording_encode_policy %d: %w", p.RecordingID, err)
		}
	}
	res.RecordingEncodePolicies = len(policies)
	return nil
}

// applyMediaAssets は media_assets を復元し、件数を res.MediaAssets に書く。
func applyMediaAssets(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, assets []catalogdoc.MediaAsset, res *store.RestoreResult, missingAssetCopyKeys map[store.MediaAssetCopyKey]struct{}, storeName string) error {
	var missingAt time.Time
	for _, a := range assets {
		assetStore := a.Store
		if assetStore == "" {
			assetStore = storeName
		}
		copyState, copyDeletedAt := a.State, a.DeletedAt
		_, observedMissing := missingAssetCopyKeys[store.MediaAssetCopyKey{MediaAssetID: a.ID, Store: assetStore, RelPath: a.RelPath}]
		fileVerifiedMissing := observedMissing && a.State != "deleted"
		if fileVerifiedMissing {
			if missingAt.IsZero() {
				if err := tx.QueryRow(ctx, "SELECT now()").Scan(&missingAt); err != nil {
					return fmt.Errorf("reading database time for missing catalog assets: %w", err)
				}
			}
			// Keep the historical copy and dependent statistics, but never publish
			// an active state for a file removed since export.
			copyState = "deleted"
			copyDeletedAt = &missingAt
			res.MissingMediaFiles++
		}
		sha256Bytes, err := decodeMediaAssetSHA256(a.SHA256)
		if err != nil {
			return fmt.Errorf("decoding media_asset %d sha256: %w", a.ID, err)
		}
		derivedGeneration := catalogDerivedGeneration(a)
		copyGeneration := catalogCopyGeneration(a)
		if err := q.CatalogUpsertMediaAsset(ctx, sqlcgen.CatalogUpsertMediaAssetParams{
			ID: a.ID, RecordingID: a.RecordingID, Kind: a.Kind, Profile: a.Profile,
			SizeBytes: a.SizeBytes,
			CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
			DerivedGeneration: derivedGeneration, Sha256: sha256Bytes, CopyGeneration: copyGeneration,
		}); err != nil {
			return fmt.Errorf("upserting media_asset %d: %w", a.ID, err)
		}
		if err := upsertCatalogMediaAssetCopy(ctx, q, sqlcgen.UpsertMediaAssetCopyParams{
			MediaAssetID: a.ID, Store: assetStore, RelPath: a.RelPath,
			Generation: copyGeneration, State: copyState, CreatedAt: &a.CreatedAt, DeletedAt: copyDeletedAt,
		}, fileVerifiedMissing); err != nil {
			return fmt.Errorf("upserting media_asset_copy for asset %d in store %q: %w", a.ID, assetStore, err)
		}
	}
	res.MediaAssets = len(assets)
	return nil
}

func upsertCatalogMediaAssetCopy(ctx context.Context, q *sqlcgen.Queries, params sqlcgen.UpsertMediaAssetCopyParams, fileVerifiedMissing bool) error {
	if params.State != "deleted" {
		rows, err := q.UpsertMediaAssetCopy(ctx, params)
		if err != nil {
			return err
		}
		if rows == 1 {
			return nil
		}
		if rows != 0 {
			return fmt.Errorf("upsert affected %d rows, want zero or one", rows)
		}
		// The row may have been deleted after rescue statted the file. The full
		// (store, rel_path) index or a migration-moved tombstone must win, so an
		// older active snapshot cannot resurrect it (TestCatalogRestoreDoesNotResurrectCopyDeletedAfterStat,
		// TestCatalogRestoreDoesNotResurrectMovedTombstoneAtReusedPath).
		identityTombstone, err := q.CatalogHasDeletedMediaAssetCopy(ctx, sqlcgen.CatalogHasDeletedMediaAssetCopyParams{
			MediaAssetID: params.MediaAssetID, Store: params.Store, Generation: params.Generation,
		})
		if err != nil {
			return err
		}
		if identityTombstone {
			return nil
		}
		tombstone, err := q.CatalogHasDeletedMediaAssetCopyAtPhysicalPath(ctx, sqlcgen.CatalogHasDeletedMediaAssetCopyAtPhysicalPathParams{
			Store: params.Store, RelPath: params.RelPath,
		})
		if err != nil {
			return err
		}
		if tombstone {
			return nil
		}
		return fmt.Errorf("upsert affected %d rows, want one", rows)
	}

	copy, err := q.LockMediaAssetCopyByPhysicalPath(ctx, sqlcgen.LockMediaAssetCopyByPhysicalPathParams{
		Store: params.Store, RelPath: params.RelPath,
	})
	if err == nil {
		matchesSnapshot := copy.MediaAssetID == params.MediaAssetID && copy.Generation == params.Generation
		if fileVerifiedMissing && matchesSnapshot {
			rows, err := q.SetMediaAssetCopyState(ctx, sqlcgen.SetMediaAssetCopyStateParams{
				ID: copy.ID, StoreName: params.Store, State: "deleted", ExpectedState: copy.State, DeletedAt: params.DeletedAt,
			})
			if err != nil {
				return err
			}
			if rows != 1 {
				return fmt.Errorf("deleting copy %d affected %d rows, want one", copy.ID, rows)
			}
			return nil
		}
		// A catalog tombstone is historical state, not proof that the current path
		// owner is missing. Preserve any live owner and restore the tombstone separately
		// (TestCatalogRestoreHistoricalTombstoneKeepsRepublishedCopyActive).
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	exists, err := q.CatalogHasDeletedMediaAssetCopy(ctx, sqlcgen.CatalogHasDeletedMediaAssetCopyParams{
		MediaAssetID: params.MediaAssetID, Store: params.Store, Generation: params.Generation,
	})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	rows, err := q.UpsertMediaAssetCopy(ctx, params)
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("insert affected %d rows, want zero or one", rows)
	}

	// Older catalogs can contain a tombstone for a path now owned by a different
	// copy. Preserve that tombstone under a reserved synthetic key; the long single
	// component cannot name a supported filesystem path or S3 object.
	params.RelPath = catalogTombstoneRelPath(params)
	rows, err = q.UpsertMediaAssetCopy(ctx, params)
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("insert at reserved path affected %d rows, want zero or one", rows)
	}
	exists, err = q.CatalogHasDeletedMediaAssetCopy(ctx, sqlcgen.CatalogHasDeletedMediaAssetCopyParams{
		MediaAssetID: params.MediaAssetID, Store: params.Store, Generation: params.Generation,
	})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return fmt.Errorf("insert at reserved path affected %d rows, want one", rows)
}

func catalogTombstoneRelPath(params sqlcgen.UpsertMediaAssetCopyParams) string {
	return strings.Repeat("t", 1025) + fmt.Sprintf("-catalog-%d-%s-%d", params.MediaAssetID, params.Store, params.Generation)
}

func decodeMediaAssetSHA256(value *string) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	decoded, err := hex.DecodeString(*value)
	if err != nil {
		return nil, err
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("got %d bytes, want 32", len(decoded))
	}
	return decoded, nil
}

func catalogDerivedGeneration(asset catalogdoc.MediaAsset) *int64 {
	if asset.Kind == "original" {
		return nil
	}
	if asset.DerivedGeneration != nil {
		return asset.DerivedGeneration
	}
	generation := legacyDerivedGeneration(asset.RelPath)
	return &generation
}

func catalogCopyGeneration(asset catalogdoc.MediaAsset) int64 {
	if asset.Kind == "original" {
		return 0
	}
	if asset.Generation != nil {
		return *asset.Generation
	}
	return legacyDerivedGeneration(asset.RelPath)
}

func legacyDerivedGeneration(relPath string) int64 {
	name := strings.TrimSuffix(path.Base(relPath), path.Ext(relPath))
	marker := max(strings.LastIndex(name, ".g"), strings.LastIndex(name, ".u"))
	if marker < 0 || marker+2 == len(name) {
		return 0
	}
	suffix := name[marker+2:]
	generation, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != suffix {
		return 0
	}
	return generation
}

// applyRecordingChapterOwnerships は recording_chapter_ownership を復元し、件数を
// res.RecordingChapterOwnerships に書く。
func applyRecordingChapterOwnerships(ctx context.Context, q *sqlcgen.Queries, rows []catalogdoc.RecordingChapterOwnership, res *store.RestoreResult) error {
	for _, o := range rows {
		if err := q.CatalogUpsertRecordingChapterOwnership(ctx, sqlcgen.CatalogUpsertRecordingChapterOwnershipParams{
			RecordingID: o.RecordingID,
			AdoptedAt:   o.AdoptedAt,
		}); err != nil {
			return fmt.Errorf("upserting recording_chapter_ownership %d: %w", o.RecordingID, err)
		}
	}
	res.RecordingChapterOwnerships = len(rows)
	return nil
}

// applyRecordingChapterSpans は recording_chapter_spans を復元し、件数を
// res.RecordingChapterSpans に書く。
//
// **足すだけで、消さない。** この表は主キーを持たず、区間の重なりを EXCLUDE が
// 禁じているので、同じ世代を 2 回当てると同じ区間が自分自身と衝突する。DO NOTHING
// で受けるので 2 回目以降は 0 件になり、rescue 全体の冪等性が保たれる（既存の
// 衛星表と同じく、dump に無い行は消さない）。
func applyRecordingChapterSpans(ctx context.Context, q *sqlcgen.Queries, rows []catalogdoc.RecordingChapterSpan, res *store.RestoreResult) error {
	for _, s := range rows {
		if err := q.CatalogInsertRecordingChapterSpan(ctx, sqlcgen.CatalogInsertRecordingChapterSpanParams{
			RecordingID: s.RecordingID,
			StartMs:     s.StartMs,
			EndMs:       s.EndMs,
			Label:       s.Label,
			Cut:         s.Cut,
		}); err != nil {
			return fmt.Errorf("inserting recording_chapter_span [%d,%d) of %d: %w", s.StartMs, s.EndMs, s.RecordingID, err)
		}
	}
	res.RecordingChapterSpans = len(rows)
	return nil
}

// applyDropStats は drop_stats を復元し、件数を res.DropStats に書く。
func applyDropStats(ctx context.Context, q *sqlcgen.Queries, stats []catalogdoc.DropStat, res *store.RestoreResult) error {
	for _, d := range stats {
		if err := q.CatalogUpsertDropStat(ctx, sqlcgen.CatalogUpsertDropStatParams{
			MediaAssetID: d.MediaAssetID,
			Pid:          d.Pid,
			Packets:      d.Packets,
			Drops:        d.Drops,
			Errors:       d.Errors,
			Scrambled:    d.Scrambled,
			PidType:      d.PidType,
		}); err != nil {
			return fmt.Errorf("upserting drop_stat asset=%d pid=%d: %w", d.MediaAssetID, d.Pid, err)
		}
	}
	res.DropStats = len(stats)
	return nil
}

// applyDropPositions は drop_positions を復元し、件数を res.DropPositions に書く。
func applyDropPositions(ctx context.Context, q *sqlcgen.Queries, positions []catalogdoc.DropPosition, res *store.RestoreResult) error {
	for _, p := range positions {
		if err := q.CatalogUpsertDropPosition(ctx, sqlcgen.CatalogUpsertDropPositionParams{
			MediaAssetID: p.MediaAssetID,
			ByteOffset:   p.ByteOffset,
			Pid:          p.Pid,
			ElapsedMs:    p.ElapsedMs,
		}); err != nil {
			return fmt.Errorf("upserting drop_position asset=%d offset=%d: %w",
				p.MediaAssetID, p.ByteOffset, err)
		}
	}
	res.DropPositions = len(positions)
	return nil
}

// applyMediaAssetTSScans は media_asset_ts_scans を復元し、件数を res.MediaAssetTSScans に書く。
func applyMediaAssetTSScans(ctx context.Context, q *sqlcgen.Queries, scans []catalogdoc.MediaAssetTSScan, res *store.RestoreResult) error {
	for _, s := range scans {
		if err := q.UpsertMediaAssetTSScan(ctx, sqlcgen.UpsertMediaAssetTSScanParams{
			MediaAssetID:     s.MediaAssetID,
			ScannedSizeBytes: s.ScannedSizeBytes,
		}); err != nil {
			return fmt.Errorf("upserting media_asset_ts_scan asset=%d: %w", s.MediaAssetID, err)
		}
	}
	res.MediaAssetTSScans = len(scans)
	return nil
}

// legacyMediaAssetTSScans は計測記録配列が無い旧 catalog の互換補完を作る。
func legacyMediaAssetTSScans(doc *catalogdoc.Document) []catalogdoc.MediaAssetTSScan {
	assetsWithStats := make(map[int64]struct{}, len(doc.DropStats))
	for _, stat := range doc.DropStats {
		assetsWithStats[stat.MediaAssetID] = struct{}{}
	}

	rows := make([]catalogdoc.MediaAssetTSScan, 0, len(assetsWithStats))
	for _, asset := range doc.MediaAssets {
		if asset.Kind != "original" {
			continue
		}
		if _, ok := assetsWithStats[asset.ID]; !ok {
			continue
		}
		rows = append(rows, catalogdoc.MediaAssetTSScan{
			MediaAssetID:     asset.ID,
			ScannedSizeBytes: asset.SizeBytes,
		})
	}
	return rows
}

func upsertRule(ctx context.Context, q *sqlcgen.Queries, rule catalogdoc.Rule) error {
	profiles := rule.EncodeProfiles
	if profiles == nil {
		profiles = []string{}
	}
	meta := rule.Metadata
	if len(meta) == 0 {
		meta = []byte("{}")
	}

	params := sqlcgen.CatalogUpsertRuleParams{
		ID:               rule.ID,
		Name:             rule.Name,
		Description:      rule.Description,
		Enabled:          rule.Enabled,
		Priority:         rule.Priority,
		DurationMinMs:    rule.DurationMinMs,
		DurationMaxMs:    rule.DurationMaxMs,
		PeriodStartAt:    rule.PeriodStartAt,
		PeriodEndAt:      rule.PeriodEndAt,
		DedupeEnabled:    rule.DedupeEnabled,
		KeepOriginal:     rule.KeepOriginal,
		EncodeProfiles:   profiles,
		FilenameTemplate: rule.FilenameTemplate,
		Metadata:         meta,
		CreatedAt:        rule.CreatedAt,
		UpdatedAt:        rule.UpdatedAt,
	}
	if rule.IsFree != nil {
		params.IsFree = pgtype.Bool{Bool: *rule.IsFree, Valid: true}
	}
	if rule.DedupeThreshold != nil {
		params.DedupeThreshold = pgtype.Float4{Float32: *rule.DedupeThreshold, Valid: true}
	}
	if rule.DedupeWindowSeconds != nil {
		params.DedupeWindow = secondsToInterval(*rule.DedupeWindowSeconds)
	}

	if err := q.CatalogUpsertRule(ctx, params); err != nil {
		return err
	}

	// 子テーブルは「一度消してから入れ直す」で削除された条件も冪等に反映する。
	if err := q.CatalogDeleteRuleTextMatches(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting text matches: %w", err)
	}
	if err := q.CatalogDeleteRuleServices(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting services: %w", err)
	}
	if err := q.CatalogDeleteRuleChannelTypes(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting channel types: %w", err)
	}
	if err := q.CatalogDeleteRuleGenres(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting genres: %w", err)
	}
	if err := q.CatalogDeleteRuleTimes(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting times: %w", err)
	}
	if err := q.CatalogDeleteRuleSites(ctx, rule.ID); err != nil {
		return fmt.Errorf("deleting sites: %w", err)
	}

	for _, m := range rule.TextMatches {
		if err := q.CatalogInsertRuleTextMatch(ctx, sqlcgen.CatalogInsertRuleTextMatchParams{
			RuleID:        rule.ID,
			Seq:           m.Seq,
			Target:        m.Target,
			Mode:          m.Mode,
			Value:         m.Value,
			CaseSensitive: m.CaseSensitive,
			Negate:        m.Negate,
		}); err != nil {
			return fmt.Errorf("inserting text match seq=%d: %w", m.Seq, err)
		}
	}
	for _, s := range rule.Services {
		if err := q.CatalogInsertRuleService(ctx, sqlcgen.CatalogInsertRuleServiceParams{
			RuleID:    rule.ID,
			NetworkID: s.NetworkID,
			ServiceID: s.ServiceID,
		}); err != nil {
			return fmt.Errorf("inserting service %d/%d: %w", s.NetworkID, s.ServiceID, err)
		}
	}
	for _, ct := range rule.ChannelTypes {
		if err := q.CatalogInsertRuleChannelType(ctx, sqlcgen.CatalogInsertRuleChannelTypeParams{
			RuleID:      rule.ID,
			ChannelType: ct,
		}); err != nil {
			return fmt.Errorf("inserting channel type %s: %w", ct, err)
		}
	}
	for _, g := range rule.Genres {
		if err := q.CatalogInsertRuleGenre(ctx, sqlcgen.CatalogInsertRuleGenreParams{
			RuleID:   rule.ID,
			GenreLv1: g,
		}); err != nil {
			return fmt.Errorf("inserting genre %d: %w", g, err)
		}
	}
	for _, t := range rule.Times {
		if err := q.CatalogInsertRuleTime(ctx, sqlcgen.CatalogInsertRuleTimeParams{
			RuleID:   rule.ID,
			Seq:      t.Seq,
			Weekdays: t.Weekdays,
			StartSec: t.StartSec,
			EndSec:   t.EndSec,
		}); err != nil {
			return fmt.Errorf("inserting time seq=%d: %w", t.Seq, err)
		}
	}
	for _, site := range rule.Sites {
		if err := q.CatalogInsertRuleSite(ctx, sqlcgen.CatalogInsertRuleSiteParams{
			RuleID: rule.ID,
			Site:   site,
		}); err != nil {
			return fmt.Errorf("inserting site %s: %w", site, err)
		}
	}
	return nil
}
