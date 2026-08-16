package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// Export は DB からコアメタデータを集めて Document を組み立てる。
// site が空なら全サイト。rules はサイト非依存なので常に全件。
//
// これは単一スナップショットからの読み取りである。recordings を読んだ後に
// media_assets / drop_stats を読むため、トランザクション無しで発行すると
// その間に作られた録画のアセットだけが media_assets 側に写り、
// RescueFile（internal/catalog/rescue.go）が recordings → media_assets の順で
// 1 トランザクション書き込むときに FK 違反でその世代がまるごと復元不能になる
// （issue #106）。これを防ぐため、Export は *pgxpool.Pool を受けて内部で
// REPEATABLE READ / READ ONLY のトランザクションを張る形に固定してある。
// 呼び出し側が任意の *sqlcgen.Queries を渡せる形にすると tx なしで呼べてしまう
// ので、シグネチャは緩めない。
func Export(ctx context.Context, pool *pgxpool.Pool, site string) (*Document, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("beginning export tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	var siteFilter *string
	if site != "" {
		siteFilter = &site
	}

	doc := &Document{
		Version:    Version,
		ExportedAt: time.Now().UTC(),
		Site:       siteFilter,
	}

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

	doc.Rules = assembleRules(rules, textMatches, services, channelTypes, genres, times, ruleSites)

	recordings, err := q.CatalogListRecordings(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing recordings: %w", err)
	}
	doc.Recordings = make([]Recording, 0, len(recordings))
	for _, r := range recordings {
		doc.Recordings = append(doc.Recordings, recordingFromRow(r))
	}

	encodePolicies, err := q.CatalogListRecordingEncodePolicies(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing recording_encode_policy: %w", err)
	}
	doc.RecordingEncodePolicies = make([]RecordingEncodePolicy, 0, len(encodePolicies))
	for _, p := range encodePolicies {
		doc.RecordingEncodePolicies = append(doc.RecordingEncodePolicies, RecordingEncodePolicy{
			RecordingID:    p.RecordingID,
			KeepOriginal:   p.KeepOriginal,
			EncodeProfiles: nonNilStrings(p.EncodeProfiles),
			CreatedAt:      p.CreatedAt,
			UpdatedAt:      p.UpdatedAt,
		})
	}

	assets, err := q.CatalogListMediaAssets(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing media_assets: %w", err)
	}
	doc.MediaAssets = make([]MediaAsset, 0, len(assets))
	for _, a := range assets {
		doc.MediaAssets = append(doc.MediaAssets, mediaAssetFromRow(a))
	}

	drops, err := q.CatalogListDropStats(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing drop_stats: %w", err)
	}
	doc.DropStats = make([]DropStat, 0, len(drops))
	for _, d := range drops {
		doc.DropStats = append(doc.DropStats, DropStat{
			MediaAssetID: d.MediaAssetID,
			Pid:          d.Pid,
			Packets:      d.Packets,
			Drops:        d.Drops,
			Errors:       d.Errors,
			Scrambled:    d.Scrambled,
			PidType:      d.PidType,
		})
	}

	snaps, err := q.CatalogListProgramSnapshots(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing program_snapshots: %w", err)
	}
	doc.ProgramSnapshots = make([]ProgramSnapshot, 0, len(snaps))
	for _, s := range snaps {
		// program_snapshots のチャンネル・イベント識別 6 列は issue #101
		// （00026）で NOT NULL 化されたが、Document 側の ProgramSnapshot は
		// ポインタのままにしてある（catalog.ProgramSnapshot のコメント参照:
		// 00026 より前に作られた catalog ダンプを rescue するときの後方互換のため）。
		// ここでは DB から読んだ非 NULL の値のアドレスを取るだけ
		// （Go 1.22+ はループ変数がイテレーションごとに新しいので安全）。
		doc.ProgramSnapshots = append(doc.ProgramSnapshots, ProgramSnapshot{
			Site:        s.Site,
			ProgramID:   s.ProgramID,
			Title:       s.Title,
			StartAt:     s.StartAt,
			DurationMs:  s.DurationMs,
			NetworkID:   &s.NetworkID,
			ServiceID:   &s.ServiceID,
			ChannelType: &s.ChannelType,
			Channel:     &s.Channel,
			EventID:     &s.EventID,
			ServiceName: &s.ServiceName,
			UpdatedAt:   s.UpdatedAt,
		})
	}

	intents, err := q.CatalogListProgramIntents(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing program_intents: %w", err)
	}
	doc.ProgramIntents = make([]ProgramIntent, 0, len(intents))
	for _, i := range intents {
		doc.ProgramIntents = append(doc.ProgramIntents, ProgramIntent{
			Site:      i.Site,
			ProgramID: i.ProgramID,
			Action:    i.Action,
			CreatedAt: i.CreatedAt,
			UpdatedAt: i.UpdatedAt,
		})
	}

	overrides, err := q.CatalogListProgramOverrides(ctx, siteFilter)
	if err != nil {
		return nil, fmt.Errorf("listing program_overrides: %w", err)
	}
	doc.ProgramOverrides = make([]ProgramOverride, 0, len(overrides))
	for _, o := range overrides {
		doc.ProgramOverrides = append(doc.ProgramOverrides, ProgramOverride{
			Site:      o.Site,
			ProgramID: o.ProgramID,
			Overrides: o.Overrides,
			CreatedAt: o.CreatedAt,
			UpdatedAt: o.UpdatedAt,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing export tx: %w", err)
	}
	return doc, nil
}

func assembleRules(
	rules []sqlcgen.Rule,
	textMatches []sqlcgen.RuleTextMatch,
	services []sqlcgen.RuleService,
	channelTypes []sqlcgen.RuleChannelType,
	genres []sqlcgen.RuleGenre,
	times []sqlcgen.RuleTime,
	ruleSites []sqlcgen.RuleSite,
) []Rule {
	out := make([]Rule, 0, len(rules))
	byID := make(map[int64]*Rule, len(rules))
	for _, r := range rules {
		rule := ruleFromRow(r)
		out = append(out, rule)
		byID[r.ID] = &out[len(out)-1]
	}

	for _, m := range textMatches {
		if rule, ok := byID[m.RuleID]; ok {
			rule.TextMatches = append(rule.TextMatches, RuleTextMatch{
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
			rule.Services = append(rule.Services, RuleService{
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
			rule.Times = append(rule.Times, RuleTime{
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

func ruleFromRow(r sqlcgen.Rule) Rule {
	rule := Rule{
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

func recordingFromRow(r sqlcgen.Recording) Recording {
	return Recording{
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
		PurgeAfter:        r.PurgeAfter,
		SupersededAt:      r.SupersededAt,
		PurgedAt:          r.PurgedAt,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func mediaAssetFromRow(a sqlcgen.MediaAsset) MediaAsset {
	return MediaAsset{
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
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func secondsToInterval(sec int64) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: sec * 1_000_000,
		Valid:        true,
	}
}

func intervalToSeconds(iv pgtype.Interval) (int64, bool) {
	if !iv.Valid {
		return 0, false
	}
	return iv.Microseconds / 1_000_000, true
}
