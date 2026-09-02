package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RescueResult は rescue で復元した件数のサマリ。
type RescueResult struct {
	CatalogPath string
	// Generation は復元に使った世代ディレクトリ名（docs/storage.md §8）。
	Generation string
	// RejectedSnapshots は完成判定に落ちて飛ばした世代（新しい順）。空でなければ
	// 「最新に見えた世代を飛ばして古い世代へ落ちた」ということなので、黙って
	// 成功させずに呼び出し側が報告する。
	RejectedSnapshots []RejectedSnapshot

	Rules                   int
	Recordings              int
	RecordingEncodePolicies int
	RecordingPurgeRequests  int
	MediaAssets             int
	DropStats               int
	ProgramSnapshots        int
	ProgramIntents          int
	ProgramOverrides        int

	// SkippedProgramSnapshots は識別子が壊れていて復元できなかった
	// program_snapshots の件数。0 でなければ依存する program_intents /
	// program_overrides も落ちている。黙って切り捨てず呼び出し側が報告する。
	SkippedProgramSnapshots int
	SkippedProgramIntents   int
	SkippedProgramOverrides int
}

// RescueLatest は media_dir/catalog/ の**最新の完成世代**を読んで DB に冪等
// upsert する（完成判定は SelectLatest / VerifyGeneration。docs/storage.md §8）。
//
// 最新世代が不完全・checksum 不一致なら 1 つ前の完成世代へ落ちる。完成世代が
// 1 つも無ければ media_dir を走査して認識できる動画ファイルを素の asset として
// in-place 登録する。どの経路もファイル本体はコピー・変更しない。
//
// registrySites は `mirakcs:` レジストリの site 名一覧。ストレージ走査で
// `sites/{site}/` 前置から site を読んだとき、その site がレジストリに
// 無ければ typo/ゴミディレクトリの疑いがあるとして Warn で目立たせる
// （classifySiteForRescuedFile 参照）。catalog JSON からの復元では使わない。
func RescueLatest(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string, registrySites []string) (*RescueResult, error) {
	sel, err := SelectLatest(mediaDir)
	if sel != nil {
		for _, r := range sel.Rejected {
			slog.Warn("rescue: skipping incomplete catalog generation",
				"generation", r.Name, "reason", r.Reason)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			// 「catalog が 1 つも無い」と「あったが全部不完全だった」を
			// 混同させない: 飛ばした世代は結果に載せて呼び出し側に報告させる。
			result, scanErr := rescueStorage(ctx, pool, mediaDir, site, registrySites)
			if scanErr != nil {
				return nil, scanErr
			}
			if sel != nil {
				result.RejectedSnapshots = sel.Rejected
			}
			return result, nil
		}
		return nil, err
	}
	result, err := RescueFile(ctx, pool, sel.DocumentPath)
	if err != nil {
		return nil, err
	}
	result.Generation = sel.Generation
	result.RejectedSnapshots = sel.Rejected
	return result, nil
}

// RescueFile は path の catalog JSON を読んで DB に冪等 upsert する。
//
// 書き込み順: rules（+ 子）→ program_snapshots → program_intents /
// program_overrides → recordings → media_assets → drop_stats。
// 全部 1 トランザクションで、途中失敗なら何も残さない。
func RescueFile(ctx context.Context, pool *pgxpool.Pool, path string) (*RescueResult, error) {
	doc, err := Load(path)
	if err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning rescue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := applyDocument(ctx, tx, doc)
	if err != nil {
		return nil, err
	}
	result.CatalogPath = path

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing rescue tx: %w", err)
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
func insertableSnapshot(s ProgramSnapshot) bool {
	switch s.ChannelType {
	case "GR", "BS", "CS", "SKY":
		return true
	default:
		return false
	}
}

func applyDocument(ctx context.Context, tx pgx.Tx, doc *Document) (*RescueResult, error) {
	q := sqlcgen.New(tx)
	res := &RescueResult{}
	// 復元できなかった program_snapshots のキー。FK 先を失う
	// program_intents / program_overrides を連動して落とすために持つ。
	skippedSnapshots := map[snapshotKey]struct{}{}

	for _, rule := range doc.Rules {
		if err := upsertRule(ctx, q, rule); err != nil {
			return nil, fmt.Errorf("upserting rule %d: %w", rule.ID, err)
		}
		res.Rules++
	}

	for _, s := range doc.ProgramSnapshots {
		// **壊れた 1 行で災害復旧そのものを止めない。**
		//
		// catalog は DB より寿命の長いバックアップファイルで、手で編集された・
		// 書き込みが途中で切れた・将来の export にバグがある、といった経路で
		// 識別子を欠く行が混ざりうる。`program_snapshots.channel_type` には
		// 列挙の CHECK があるので、そのまま INSERT すると**トランザクションごと
		// ロールバックして recordings も media_assets も 1 件も復元されない**
		// （実測: `channel_type: null` の 1 行で
		// TestApplyDocument_MalformedSnapshotIsSkipped の対象になった挙動）。
		//
		// program_snapshots は放送 + epg.retention_grace（既定 24h）で GC される
		// 導出テーブルで、rescue が守るべき永続資産（recordings / media_assets /
		// drop_stats / rules）はこのループより後に復元される。1 行の導出データの
		// ために全部を失うのは釣り合わない --- 行を落として続行し、件数を
		// RescueResult に数えて呼び出し側に報告させる（黙って切り捨てない）。
		if !insertableSnapshot(s) {
			slog.Warn("rescue: skipping program_snapshot the database would reject",
				"site", s.Site, "program_id", s.ProgramID, "channel_type", s.ChannelType)
			skippedSnapshots[snapshotKey{site: s.Site, programID: s.ProgramID}] = struct{}{}
			res.SkippedProgramSnapshots++
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
		res.ProgramSnapshots++
	}

	for _, i := range doc.ProgramIntents {
		// FK 先の program_snapshots を落としたものは一緒に落とす（上記参照）。
		if _, skipped := skippedSnapshots[snapshotKey{site: i.Site, programID: i.ProgramID}]; skipped {
			res.SkippedProgramIntents++
			continue
		}
		if err := q.CatalogUpsertProgramIntent(ctx, sqlcgen.CatalogUpsertProgramIntentParams{
			Site:      i.Site,
			ProgramID: i.ProgramID,
			Action:    i.Action,
			CreatedAt: i.CreatedAt,
			UpdatedAt: i.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting program_intent %s/%d: %w", i.Site, i.ProgramID, err)
		}
		res.ProgramIntents++
	}

	for _, o := range doc.ProgramOverrides {
		// FK 先の program_snapshots を落としたものは一緒に落とす（上記参照）。
		if _, skipped := skippedSnapshots[snapshotKey{site: o.Site, programID: o.ProgramID}]; skipped {
			res.SkippedProgramOverrides++
			continue
		}
		if err := q.CatalogUpsertProgramOverride(ctx, sqlcgen.CatalogUpsertProgramOverrideParams{
			Site:      o.Site,
			ProgramID: o.ProgramID,
			Overrides: o.Overrides,
			CreatedAt: o.CreatedAt,
			UpdatedAt: o.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting program_override %s/%d: %w", o.Site, o.ProgramID, err)
		}
		res.ProgramOverrides++
	}

	for _, r := range doc.Recordings {
		qe := r.QualityEvents
		if len(qe) == 0 {
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
			return nil, fmt.Errorf("upserting recording %d: %w", r.ID, err)
		}
		res.Recordings++
	}

	// recording_purge_requests は recordings への FK を持つので recordings の
	// upsert より後に書く。doc.RecordingPurgeRequests に載っていない録画には
	// 何も書かない --- 「即時削除の要求は無い」は行の不在そのものが意味を持つ
	// （不変条件 10）。
	for _, p := range doc.RecordingPurgeRequests {
		if err := q.CatalogUpsertRecordingPurgeRequest(ctx, sqlcgen.CatalogUpsertRecordingPurgeRequestParams{
			RecordingID: p.RecordingID,
			RequestedAt: p.RequestedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting recording_purge_request %d: %w", p.RecordingID, err)
		}
		res.RecordingPurgeRequests++
	}

	// recording_encode_policy（issue #159）は recordings への FK を持つので
	// recordings の upsert より後に書く。doc.RecordingEncodePolicies に
	// 載っていない録画には何も書かない --- 「未凍結」は行の不在そのものが
	// 意味を持つ（不変条件 10）ので、既定値の行で埋めると凍結済みと誤認する。
	for _, p := range doc.RecordingEncodePolicies {
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
		}); err != nil {
			return nil, fmt.Errorf("upserting recording_encode_policy %d: %w", p.RecordingID, err)
		}
		res.RecordingEncodePolicies++
	}

	for _, a := range doc.MediaAssets {
		if err := q.CatalogUpsertMediaAsset(ctx, sqlcgen.CatalogUpsertMediaAssetParams{
			ID:          a.ID,
			RecordingID: a.RecordingID,
			Kind:        a.Kind,
			Profile:     a.Profile,
			RelPath:     a.RelPath,
			SizeBytes:   a.SizeBytes,
			State:       a.State,
			DeletedAt:   a.DeletedAt,
			CreatedAt:   a.CreatedAt,
			UpdatedAt:   a.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting media_asset %d: %w", a.ID, err)
		}
		res.MediaAssets++
	}

	for _, d := range doc.DropStats {
		if err := q.CatalogUpsertDropStat(ctx, sqlcgen.CatalogUpsertDropStatParams{
			MediaAssetID: d.MediaAssetID,
			Pid:          d.Pid,
			Packets:      d.Packets,
			Drops:        d.Drops,
			Errors:       d.Errors,
			Scrambled:    d.Scrambled,
			PidType:      d.PidType,
		}); err != nil {
			return nil, fmt.Errorf("upserting drop_stat asset=%d pid=%d: %w", d.MediaAssetID, d.Pid, err)
		}
		res.DropStats++
	}

	if err := q.CatalogResetRulesIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting rules id seq: %w", err)
	}
	if err := q.CatalogResetRecordingsIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting recordings id seq: %w", err)
	}
	if err := q.CatalogResetMediaAssetsIDSeq(ctx); err != nil {
		return nil, fmt.Errorf("resetting media_assets id seq: %w", err)
	}

	return res, nil
}

func upsertRule(ctx context.Context, q *sqlcgen.Queries, rule Rule) error {
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
