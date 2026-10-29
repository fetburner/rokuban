package pgstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// Export は DB からコアメタデータを集めて catalogdoc.Document を組み立てる。常に全サイト
// （catalog は災害復旧用のアーカイブ全体を対象にする。site 絞り込みは書き手も
// 呼び手も持たなかったため issue #441 で落とした）。
//
// これは単一スナップショットからの読み取りである。recordings を読んだ後に
// media_assets / drop_stats / drop_positions / media_asset_ts_scans を読むため、トランザクション無しで発行すると
// その間に作られた録画のアセットだけが media_assets 側に写り、
// Catalog.Restore（internal/store/pgstore/catalog_restore.go）が recordings → media_assets の順で
// 1 トランザクション書き込むときに FK 違反でその世代がまるごと復元不能になる
// （issue #106）。Export は内部で REPEATABLE READ / READ ONLY のトランザクションを
// 開き、常に同じスナップショットから読み取る。
func (s *Catalog) Export(ctx context.Context) (*catalogdoc.Document, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("beginning export tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	doc := &catalogdoc.Document{
		Version:    catalogdoc.Version,
		ExportedAt: time.Now().UTC(),
	}

	if doc.Rules, err = exportRules(ctx, q); err != nil {
		return nil, err
	}
	if doc.LabelRules, err = exportLabelRules(ctx, q); err != nil {
		return nil, err
	}
	if doc.ProgramSnapshots, err = exportProgramSnapshots(ctx, q); err != nil {
		return nil, err
	}
	if doc.ProgramIntents, err = exportProgramIntents(ctx, q); err != nil {
		return nil, err
	}
	if doc.ProgramOverrides, err = exportProgramOverrides(ctx, q); err != nil {
		return nil, err
	}
	if doc.Recordings, err = exportRecordings(ctx, q); err != nil {
		return nil, err
	}
	if doc.RecordingPurgeRequests, err = exportRecordingPurgeRequests(ctx, q); err != nil {
		return nil, err
	}
	if doc.RecordingWatched, err = exportRecordingWatched(ctx, q); err != nil {
		return nil, err
	}
	if doc.RecordingEncodePolicies, err = exportRecordingEncodePolicies(ctx, q); err != nil {
		return nil, err
	}
	if doc.MediaAssets, err = exportMediaAssets(ctx, q, s.storeName); err != nil {
		return nil, err
	}
	if doc.DropStats, err = exportDropStats(ctx, q); err != nil {
		return nil, err
	}
	if doc.DropPositions, err = exportDropPositions(ctx, q); err != nil {
		return nil, err
	}
	if doc.MediaAssetTSScans, err = exportMediaAssetTSScans(ctx, q); err != nil {
		return nil, err
	}
	if doc.RecordingChapterOwnerships, err = exportRecordingChapterOwnerships(ctx, q); err != nil {
		return nil, err
	}
	if doc.RecordingChapterSpans, err = exportRecordingChapterSpans(ctx, q); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing export tx: %w", err)
	}
	return doc, nil
}

// exportLabelRules は label_rules を文書の型付き行に変換する。
func exportLabelRules(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.LabelRule, error) {
	rows, err := q.CatalogListLabelRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing label_rules: %w", err)
	}
	out := make([]catalogdoc.LabelRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, catalogdoc.LabelRule{
			ID: r.ID, Key: r.Key, Value: r.Value, Keyword: r.Keyword,
			Priority: r.Priority, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// exportRules は rules とその従属表を読み取り、文書の catalogdoc.Rule に組み立てる。
func exportRules(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.Rule, error) {
	rules, err := q.CatalogListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	textMatches, err := q.CatalogListRuleTextMatches(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_text_matches: %w", err)
	}
	services, err := q.CatalogListRuleServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_services: %w", err)
	}
	channelTypes, err := q.CatalogListRuleChannelTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_channel_types: %w", err)
	}
	genres, err := q.CatalogListRuleGenres(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_genres: %w", err)
	}
	times, err := q.CatalogListRuleTimes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_times: %w", err)
	}
	ruleSites, err := q.CatalogListRuleSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rule_sites: %w", err)
	}
	return assembleRules(rules, textMatches, services, channelTypes, genres, times, ruleSites), nil
}

// exportProgramSnapshots は program_snapshots を文書の型付き行に変換する。
func exportProgramSnapshots(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.ProgramSnapshot, error) {
	rows, err := q.CatalogListProgramSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing program_snapshots: %w", err)
	}
	out := make([]catalogdoc.ProgramSnapshot, 0, len(rows))
	for _, s := range rows {
		out = append(out, catalogdoc.ProgramSnapshot{
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
		})
	}
	return out, nil
}

// exportProgramIntents は program_intents を文書の型付き行に変換する。
func exportProgramIntents(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.ProgramIntent, error) {
	rows, err := q.CatalogListProgramIntents(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing program_intents: %w", err)
	}
	out := make([]catalogdoc.ProgramIntent, 0, len(rows))
	for _, i := range rows {
		out = append(out, catalogdoc.ProgramIntent{
			Site:      i.Site,
			ProgramID: i.ProgramID,
			Action:    i.Action,
			CreatedAt: i.CreatedAt,
			UpdatedAt: i.UpdatedAt,
		})
	}
	return out, nil
}

// exportProgramOverrides は program_overrides を文書の型付き行に変換する。
func exportProgramOverrides(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.ProgramOverride, error) {
	rows, err := q.CatalogListProgramOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing program_overrides: %w", err)
	}
	out := make([]catalogdoc.ProgramOverride, 0, len(rows))
	for _, o := range rows {
		out = append(out, catalogdoc.ProgramOverride{
			Site:      o.Site,
			ProgramID: o.ProgramID,
			Overrides: o.Overrides,
			CreatedAt: o.CreatedAt,
			UpdatedAt: o.UpdatedAt,
		})
	}
	return out, nil
}

// exportRecordings は recordings を文書の型付き行に変換する。
func exportRecordings(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.Recording, error) {
	rows, err := q.CatalogListRecordings(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recordings: %w", err)
	}
	out := make([]catalogdoc.Recording, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordingFromRow(r))
	}
	return out, nil
}

// exportRecordingPurgeRequests は recording_purge_requests を文書の型付き行に変換する。
func exportRecordingPurgeRequests(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.RecordingPurgeRequest, error) {
	rows, err := q.CatalogListRecordingPurgeRequests(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording_purge_requests: %w", err)
	}
	out := make([]catalogdoc.RecordingPurgeRequest, 0, len(rows))
	for _, p := range rows {
		out = append(out, catalogdoc.RecordingPurgeRequest{
			RecordingID: p.RecordingID,
			RequestedAt: p.RequestedAt,
		})
	}
	return out, nil
}

func exportRecordingWatched(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.RecordingWatched, error) {
	rows, err := q.CatalogListRecordingWatched(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording_watched: %w", err)
	}
	out := make([]catalogdoc.RecordingWatched, 0, len(rows))
	for _, row := range rows {
		out = append(out, catalogdoc.RecordingWatched{RecordingID: row.RecordingID, WatchedAt: row.WatchedAt})
	}
	return out, nil
}

// exportRecordingEncodePolicies は recording_encode_policy を文書の型付き行に変換する。
func exportRecordingEncodePolicies(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.RecordingEncodePolicy, error) {
	rows, err := q.CatalogListRecordingEncodePolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording_encode_policy: %w", err)
	}
	out := make([]catalogdoc.RecordingEncodePolicy, 0, len(rows))
	for _, p := range rows {
		out = append(out, catalogdoc.RecordingEncodePolicy{
			RecordingID:    p.RecordingID,
			KeepOriginal:   p.KeepOriginal,
			EncodeProfiles: nonNilStrings(p.EncodeProfiles),
			CMDetect:       p.CmDetect,
			CreatedAt:      p.CreatedAt,
			UpdatedAt:      p.UpdatedAt,
		})
	}
	return out, nil
}

// exportMediaAssets は media_assets を文書の型付き行に変換する。
func exportMediaAssets(ctx context.Context, q *sqlcgen.Queries, storeName string) ([]catalogdoc.MediaAsset, error) {
	rows, err := q.CatalogListMediaAssets(ctx, storeName)
	if err != nil {
		return nil, fmt.Errorf("listing media_assets: %w", err)
	}
	out := make([]catalogdoc.MediaAsset, 0, len(rows))
	for _, a := range rows {
		out = append(out, mediaAssetFromRow(a))
	}
	return out, nil
}

// exportDropStats は drop_stats を文書の型付き行に変換する。
func exportDropStats(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.DropStat, error) {
	rows, err := q.CatalogListDropStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing drop_stats: %w", err)
	}
	out := make([]catalogdoc.DropStat, 0, len(rows))
	for _, d := range rows {
		out = append(out, catalogdoc.DropStat{
			MediaAssetID: d.MediaAssetID,
			Pid:          d.Pid,
			Packets:      d.Packets,
			Drops:        d.Drops,
			Errors:       d.Errors,
			Scrambled:    d.Scrambled,
			PidType:      d.PidType,
		})
	}
	return out, nil
}

// exportDropPositions は drop_positions を文書の型付き行に変換する。
func exportDropPositions(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.DropPosition, error) {
	rows, err := q.CatalogListDropPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing drop_positions: %w", err)
	}
	out := make([]catalogdoc.DropPosition, 0, len(rows))
	for _, p := range rows {
		out = append(out, catalogdoc.DropPosition{
			MediaAssetID: p.MediaAssetID,
			ByteOffset:   p.ByteOffset,
			Pid:          p.Pid,
			ElapsedMs:    p.ElapsedMs,
		})
	}
	return out, nil
}

// exportMediaAssetTSScans は media_asset_ts_scans を文書の型付き行に変換する。
func exportMediaAssetTSScans(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.MediaAssetTSScan, error) {
	rows, err := q.CatalogListMediaAssetTSScans(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing media_asset_ts_scans: %w", err)
	}
	out := make([]catalogdoc.MediaAssetTSScan, 0, len(rows))
	for _, s := range rows {
		out = append(out, catalogdoc.MediaAssetTSScan{
			MediaAssetID:     s.MediaAssetID,
			ScannedSizeBytes: s.ScannedSizeBytes,
		})
	}
	return out, nil
}

// exportRecordingChapterOwnerships は recording_chapter_ownership を文書の型付き行に変換する。
func exportRecordingChapterOwnerships(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.RecordingChapterOwnership, error) {
	rows, err := q.CatalogListRecordingChapterOwnerships(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording_chapter_ownership: %w", err)
	}
	out := make([]catalogdoc.RecordingChapterOwnership, 0, len(rows))
	for _, o := range rows {
		out = append(out, catalogdoc.RecordingChapterOwnership{
			RecordingID: o.RecordingID,
			AdoptedAt:   o.AdoptedAt,
		})
	}
	return out, nil
}

// exportRecordingChapterSpans は recording_chapter_spans を文書の型付き行に変換する。
func exportRecordingChapterSpans(ctx context.Context, q *sqlcgen.Queries) ([]catalogdoc.RecordingChapterSpan, error) {
	rows, err := q.CatalogListRecordingChapterSpans(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing recording_chapter_spans: %w", err)
	}
	out := make([]catalogdoc.RecordingChapterSpan, 0, len(rows))
	for _, s := range rows {
		out = append(out, catalogdoc.RecordingChapterSpan{
			RecordingID: s.RecordingID,
			StartMs:     s.StartMs,
			EndMs:       s.EndMs,
			Label:       s.Label,
			Cut:         s.Cut,
		})
	}
	return out, nil
}

func assembleRules(
	rules []sqlcgen.Rule,
	textMatches []sqlcgen.RuleTextMatch,
	services []sqlcgen.RuleService,
	channelTypes []sqlcgen.RuleChannelType,
	genres []sqlcgen.RuleGenre,
	times []sqlcgen.RuleTime,
	ruleSites []sqlcgen.RuleSite,
) []catalogdoc.Rule {
	out := make([]catalogdoc.Rule, 0, len(rules))
	byID := make(map[int64]*catalogdoc.Rule, len(rules))
	for _, r := range rules {
		rule := ruleFromRow(r)
		out = append(out, rule)
		byID[r.ID] = &out[len(out)-1]
	}

	for _, m := range textMatches {
		if rule, ok := byID[m.RuleID]; ok {
			rule.TextMatches = append(rule.TextMatches, catalogdoc.RuleTextMatch{
				Seq:           m.Seq,
				Target:        m.Target,
				Mode:          m.Mode,
				Value:         m.Value,
				CaseSensitive: m.CaseSensitive,
				Negate:        m.Negate,
			})
		}
	}
	for _, s := range services {
		if rule, ok := byID[s.RuleID]; ok {
			rule.Services = append(rule.Services, catalogdoc.RuleService{
				NetworkID: s.NetworkID,
				ServiceID: s.ServiceID,
			})
		}
	}
	for _, c := range channelTypes {
		if rule, ok := byID[c.RuleID]; ok {
			rule.ChannelTypes = append(rule.ChannelTypes, c.ChannelType)
		}
	}
	for _, g := range genres {
		if rule, ok := byID[g.RuleID]; ok {
			rule.Genres = append(rule.Genres, g.GenreLv1)
		}
	}
	for _, t := range times {
		if rule, ok := byID[t.RuleID]; ok {
			rule.Times = append(rule.Times, catalogdoc.RuleTime{
				Seq:      t.Seq,
				Weekdays: t.Weekdays,
				StartSec: t.StartSec,
				EndSec:   t.EndSec,
			})
		}
	}
	for _, s := range ruleSites {
		if rule, ok := byID[s.RuleID]; ok {
			rule.Sites = append(rule.Sites, s.Site)
		}
	}
	return out
}

func ruleFromRow(r sqlcgen.Rule) catalogdoc.Rule {
	rule := catalogdoc.Rule{
		ID:               r.ID,
		Name:             r.Name,
		Description:      r.Description,
		Enabled:          r.Enabled,
		Priority:         r.Priority,
		DurationMinMs:    r.DurationMinMs,
		DurationMaxMs:    r.DurationMaxMs,
		PeriodStartAt:    r.PeriodStartAt,
		PeriodEndAt:      r.PeriodEndAt,
		DedupeEnabled:    r.DedupeEnabled,
		KeepOriginal:     r.KeepOriginal,
		EncodeProfiles:   nonNilStrings(r.EncodeProfiles),
		FilenameTemplate: r.FilenameTemplate,
		Metadata:         r.Metadata,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
	if r.IsFree.Valid {
		v := r.IsFree.Bool
		rule.IsFree = &v
	}
	if r.DedupeThreshold.Valid {
		v := r.DedupeThreshold.Float32
		rule.DedupeThreshold = &v
	}
	if secs, ok := intervalToSeconds(r.DedupeWindow); ok {
		rule.DedupeWindowSeconds = &secs
	}
	return rule
}

func recordingFromRow(r sqlcgen.Recording) catalogdoc.Recording {
	return catalogdoc.Recording{
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
		QualityEvents:     r.QualityEvents,
		DeletedAt:         r.DeletedAt,
		SupersededAt:      r.SupersededAt,
		PurgedAt:          r.PurgedAt,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func mediaAssetFromRow(a sqlcgen.CatalogListMediaAssetsRow) catalogdoc.MediaAsset {
	var derivedGeneration *int64
	if a.Kind != "original" {
		derivedGeneration = int64Pointer(a.DerivedGeneration)
	}
	return catalogdoc.MediaAsset{
		ID:                a.ID,
		RecordingID:       a.RecordingID,
		Kind:              a.Kind,
		Profile:           a.Profile,
		RelPath:           a.RelPath,
		SizeBytes:         a.SizeBytes,
		State:             a.State,
		DeletedAt:         a.DeletedAt,
		CreatedAt:         a.CreatedAt,
		UpdatedAt:         a.UpdatedAt,
		DerivedGeneration: derivedGeneration,
		Generation:        int64Pointer(a.Generation),
		SHA256:            sha256String(a.Sha256),
		Store:             a.Store,
	}
}

func int64Pointer(value int64) *int64 { return &value }

func sha256String(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	value := hex.EncodeToString(raw)
	return &value
}
