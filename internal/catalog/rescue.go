package catalog

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RescueResult は rescue で復元した件数のサマリ。
type RescueResult struct {
	CatalogPath      string
	Rules            int
	Recordings       int
	MediaAssets      int
	DropStats        int
	ProgramSnapshots int
	ProgramIntents   int
	ProgramOverrides int
}

// RescueLatest は media_dir/catalog/ の最新 catalog を読んで DB に冪等 upsert する。
// catalog が無ければ media_dir を走査し、認識できる動画ファイルを素の asset として
// in-place 登録する。どちらの経路もファイル本体はコピー・変更しない。
func RescueLatest(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string) (*RescueResult, error) {
	path, err := LatestPath(mediaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rescueStorage(ctx, pool, mediaDir, site)
		}
		return nil, err
	}
	return RescueFile(ctx, pool, path)
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

func applyDocument(ctx context.Context, tx pgx.Tx, doc *Document) (*RescueResult, error) {
	q := sqlcgen.New(tx)
	res := &RescueResult{}

	for _, rule := range doc.Rules {
		if err := upsertRule(ctx, q, rule); err != nil {
			return nil, fmt.Errorf("upserting rule %d: %w", rule.ID, err)
		}
		res.Rules++
	}

	for _, s := range doc.ProgramSnapshots {
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
			UpdatedAt:   s.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting program_snapshot %s/%d: %w", s.Site, s.ProgramID, err)
		}
		res.ProgramSnapshots++
	}

	for _, i := range doc.ProgramIntents {
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
		profiles := r.EncodeProfiles
		if profiles == nil {
			profiles = []string{}
		}
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
			KeepOriginal:      r.KeepOriginal,
			EncodeProfiles:    profiles,
			QualityEvents:     qe,
			DeletedAt:         r.DeletedAt,
			PurgeAfter:        r.PurgeAfter,
			SupersededAt:      r.SupersededAt,
			CreatedAt:         r.CreatedAt,
			UpdatedAt:         r.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("upserting recording %d: %w", r.ID, err)
		}
		res.Recordings++
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
