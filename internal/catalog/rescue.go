package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RescueResult は rescue で復元した件数のサマリ。
type RescueResult struct {
	CatalogPath string
	// Generation は復元に使った世代ディレクトリ名（docs/storage.md §8）。
	// 旧形式のフラットファイルから復元したときは空。
	Generation string
	// LegacyCatalog は manifest を持たない旧形式のフラットファイルから復元した
	// ことを示す。完成を検証できていないので、呼び出し側は必ず報告する。
	LegacyCatalog bool
	// RejectedSnapshots は完成判定に落ちて飛ばした世代（新しい順）。空でなければ
	// 「最新に見えた世代を飛ばして古い世代へ落ちた」ということなので、黙って
	// 成功させずに呼び出し側が報告する。
	RejectedSnapshots []RejectedSnapshot

	Rules                   int
	Recordings              int
	RecordingEncodePolicies int
	MediaAssets             int
	DropStats               int
	ProgramSnapshots        int
	ProgramIntents          int
	ProgramOverrides        int

	// SkippedProgramSnapshots は識別子を持たない（#101 / 00026 より前に export
	// された）ために復元をスキップした program_snapshots の件数。0 でない場合は
	// 依存する program_intents / program_overrides も落ちている
	// （SkippedProgramIntents / SkippedProgramOverrides）。黙って落とさず
	// 呼び出し側が報告できるように数える。
	SkippedProgramSnapshots int
	SkippedProgramIntents   int
	SkippedProgramOverrides int

	// RestoredLegacyEncodePolicies は #159 より前に export された catalog ダンプ
	// （Recording.KeepOriginalLegacy が non-nil）から復元した recording_encode_policy
	// の件数。RecordingEncodePolicies にも含まれる（内訳として別に数える）。
	RestoredLegacyEncodePolicies int

	// RestoredLegacyPurgeRequests は #319 より前に export された catalog ダンプ
	// （Recording.PurgeAfterLegacy が non-nil）から PurgeRequested へ前送りした
	// 件数。黙って切り捨てず、呼び出し側が報告できるように数える。
	RestoredLegacyPurgeRequests int
}

// RescueLatest は media_dir/catalog/ の**最新の完成世代**を読んで DB に冪等
// upsert する（完成判定は SelectLatest / VerifyGeneration。docs/storage.md §8）。
//
// 最新世代が不完全・checksum 不一致なら 1 つ前の完成世代へ落ちる。完成世代が
// 無ければ manifest を持たない旧形式のフラットファイル、それも無ければ media_dir
// を走査して認識できる動画ファイルを素の asset として in-place 登録する。
// どの経路もファイル本体はコピー・変更しない。
func RescueLatest(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string) (*RescueResult, error) {
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
			result, scanErr := rescueStorage(ctx, pool, mediaDir, site)
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
	if sel.Legacy {
		slog.Warn("rescue: falling back to a catalog file without a manifest "+
			"(completeness cannot be verified)", "path", sel.DocumentPath)
	}

	result, err := RescueFile(ctx, pool, sel.DocumentPath)
	if err != nil {
		return nil, err
	}
	result.Generation = sel.Generation
	result.LegacyCatalog = sel.Legacy
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

// snapshotKey は program_snapshots の主キー（site, program_id）。復元をスキップした
// スナップショットに紐づく program_intents / program_overrides を落とすのに使う。
type snapshotKey struct {
	site      string
	programID int64
}

func applyDocument(ctx context.Context, tx pgx.Tx, doc *Document) (*RescueResult, error) {
	q := sqlcgen.New(tx)
	res := &RescueResult{}
	// 識別子を持たない（#101 / 00026 より前の）program_snapshots をスキップした
	// キーの集合。FK 先を失う program_intents / program_overrides を連動して
	// 落とすために使う。
	skippedSnapshots := map[snapshotKey]struct{}{}

	for _, rule := range doc.Rules {
		if err := upsertRule(ctx, q, rule); err != nil {
			return nil, fmt.Errorf("upserting rule %d: %w", rule.ID, err)
		}
		res.Rules++
	}

	for _, s := range doc.ProgramSnapshots {
		// program_snapshots のチャンネル・イベント識別 6 列は issue #101（00026）で
		// NOT NULL 化されたが、catalog document 自体は DB より寿命が長い
		// バックアップファイルなので、00026 より前に export された古い
		// ダンプは依然 nil を持ちうる（document.go の ProgramSnapshot コメント参照）。
		//
		// **この行だけスキップして続行する。エラーで rescue 全体を止めない。**
		// program_snapshots は放送 + epg.retention_grace（既定 24h）で GC される
		// 導出テーブルであり、識別子を持たないほど古いダンプの行は復元しても
		// 次の GC で消える。一方 rescue が守るべきものは永続資産
		// （recordings / media_assets / drop_stats / rules）で、それらはこの
		// ループより後に復元される。1 行の導出データのために災害復旧そのものを
		// 止めるのは釣り合わない。
		//
		// program_intents / program_overrides は program_snapshots への FK を
		// 持つので、スキップした (site, program_id) に紐づくものは後続の
		// ループでも落とす（FK 違反で 1 トランザクション全体が壊れるのを防ぐ）。
		// 落とした件数は RescueResult に数えて呼び出し側が報告できるようにする
		// （黙って切り捨てない）。
		if s.NetworkID == nil || s.ServiceID == nil || s.ChannelType == nil ||
			s.Channel == nil || s.EventID == nil || s.ServiceName == nil {
			slog.Warn("rescue: skipping program_snapshot without channel/event identity "+
				"(catalog dump predates issue #101)",
				"site", s.Site, "program_id", s.ProgramID)
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
			NetworkID:   *s.NetworkID,
			ServiceID:   *s.ServiceID,
			ChannelType: *s.ChannelType,
			Channel:     *s.Channel,
			EventID:     *s.EventID,
			ServiceName: *s.ServiceName,
			UpdatedAt:   s.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting program_snapshot %s/%d: %w", s.Site, s.ProgramID, err)
		}
		res.ProgramSnapshots++
	}

	for _, i := range doc.ProgramIntents {
		// FK 先の program_snapshots をスキップしたものは一緒に落とす（上記参照）。
		if _, skipped := skippedSnapshots[snapshotKey{site: i.Site, programID: i.ProgramID}]; skipped {
			slog.Warn("rescue: skipping program_intent whose program_snapshot was skipped",
				"site", i.Site, "program_id", i.ProgramID)
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
		// FK 先の program_snapshots をスキップしたものは一緒に落とす（上記参照）。
		if _, skipped := skippedSnapshots[snapshotKey{site: o.Site, programID: o.ProgramID}]; skipped {
			slog.Warn("rescue: skipping program_override whose program_snapshot was skipped",
				"site", o.Site, "program_id", o.ProgramID)
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
		// 旧カタログの never-scheduled 擬似行は recordings に戻さない。欠測は
		// issue #318 で never_scheduled_events 表へ移設され、recordings は観測
		// された試行だけを持つ。旧擬似行は media_assets を持たない契約なので、
		// ここでスキップしても復元すべきファイルを失わない。
		if hasNeverScheduledMarker(qe) {
			continue
		}
		// #319 より前に export されたダンプは PurgeRequested を持たず（キーが
		// 無いので unmarshal 後もゼロ値の false のまま）、旧列の値は
		// PurgeAfterLegacy に残っている。前送りしないと「今すぐ完全削除」の
		// 要求が黙って失われる（migration 00039 backfill と同じ基準: 値では
		// なく non-nil かどうかだけを見る）。
		purgeRequested := r.PurgeRequested
		if !purgeRequested && r.PurgeAfterLegacy != nil {
			purgeRequested = true
			res.RestoredLegacyPurgeRequests++
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
			PurgeRequested:    purgeRequested,
			SupersededAt:      r.SupersededAt,
			PurgedAt:          r.PurgedAt,
			CreatedAt:         r.CreatedAt,
			UpdatedAt:         r.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting recording %d: %w", r.ID, err)
		}
		res.Recordings++
	}

	// recording_encode_policy（issue #159）は recordings への FK を持つので
	// recordings の upsert より後に書く。doc.RecordingEncodePolicies に
	// 載っていない録画には何も書かない --- 「未凍結」は行の不在そのものが
	// 意味を持つ（不変条件 10）ので、既定値の行で埋めると凍結済みと誤認する。
	explicitPolicies := map[int64]struct{}{}
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
		explicitPolicies[p.RecordingID] = struct{}{}
	}

	// #159 より前に export された catalog ダンプ（doc.RecordingEncodePolicies が
	// 空。旧列 recordings.keep_original / encode_profiles の値が
	// Recording.KeepOriginalLegacy / EncodeProfilesLegacy に残っている。
	// document.go 参照）を rescue するときの後方互換。何もしないと、この
	// ダンプの全録画で凍結済みポリシーが黙って失われる
	// （削除エンジンが対象外になり、EnqueueMissingEncodes が no-op になり、
	// 事後追加 API が既定値 'always' で上書きする）。
	//
	// migration 00032 backfill と同じ基準（原本 media_asset の有無で「凍結済みか」
	// を判定する。列の値そのものは使わない。不変条件 9）を、DB ではなく
	// このダンプ自身の doc.MediaAssets に対して適用する。
	originalAssetRecordingIDs := map[int64]struct{}{}
	for _, a := range doc.MediaAssets {
		if a.Kind == "original" {
			originalAssetRecordingIDs[a.RecordingID] = struct{}{}
		}
	}
	for _, r := range doc.Recordings {
		if _, ok := explicitPolicies[r.ID]; ok {
			continue // 新しいダンプで既に明示的な行がある
		}
		if r.KeepOriginalLegacy == nil {
			continue // 新しいダンプ（#159 以降）。旧列自体が無い
		}
		if _, hasOriginal := originalAssetRecordingIDs[r.ID]; !hasOriginal {
			continue // 未凍結（原本が無い）。既定値の行で埋めない
		}
		profiles := r.EncodeProfilesLegacy
		if profiles == nil {
			profiles = []string{}
		}
		if err := q.CatalogUpsertRecordingEncodePolicy(ctx, sqlcgen.CatalogUpsertRecordingEncodePolicyParams{
			RecordingID:    r.ID,
			KeepOriginal:   *r.KeepOriginalLegacy,
			EncodeProfiles: profiles,
			CreatedAt:      r.CreatedAt,
			UpdatedAt:      r.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("restoring legacy recording_encode_policy %d: %w", r.ID, err)
		}
		res.RecordingEncodePolicies++
		res.RestoredLegacyEncodePolicies++
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

// hasNeverScheduledMarker は quality_events の配列要素に
// db.QualityEventNeverScheduled マーカーがあるかを判定する。
//
// issue #318 より前に export された catalog ダンプでは、欠測が recordings の
// failed 擬似行 + このマーカーとして残っている。この関数で検出した旧擬似行は
// applyDocument が recordings に戻さずスキップする。壊れた JSON（手で編集された
// 等）は false を返して通常の録画として復元を試みる --- ここで rescue 全体を
// 止めるのは、1 件の不透明な quality_events のために他の永続資産の復旧を止める
// 代償に見合わない。
func hasNeverScheduledMarker(qualityEvents json.RawMessage) bool {
	if len(qualityEvents) == 0 {
		return false
	}
	var events []db.QualityEvent
	if err := json.Unmarshal(qualityEvents, &events); err != nil {
		return false
	}
	for _, e := range events {
		if e.Event == db.QualityEventNeverScheduled {
			return true
		}
	}
	return false
}
