package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
)

// Export は catalog の全保護対象を 1 つのスナップショットから読んで文書にする。
// 検査: contracttest.RunCatalog の「export uses one snapshot during concurrent ingest」。
// 全 site・全 store の内容を含む（Postgres 版と同じ）。物理コピーは s.storeName の置き場から読む。
func (s *Catalog) Export(ctx context.Context) (*catalogdoc.Document, error) {
	doc := &catalogdoc.Document{Version: catalogdoc.Version, ExportedAt: time.Now().UTC()}
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		if doc.Rules, err = exportRules(ctx, tx); err != nil {
			return err
		}
		if doc.RuleImportKeys, err = exportRuleImportKeys(ctx, tx); err != nil {
			return err
		}
		if doc.LabelRules, err = exportLabelRules(ctx, tx); err != nil {
			return err
		}
		if doc.ProgramSnapshots, err = exportProgramSnapshots(ctx, tx); err != nil {
			return err
		}
		if doc.ProgramIntents, err = exportProgramIntents(ctx, tx); err != nil {
			return err
		}
		if doc.ProgramOverrides, err = exportProgramOverrides(ctx, tx); err != nil {
			return err
		}
		if doc.Recordings, err = exportRecordings(ctx, tx); err != nil {
			return err
		}
		if doc.RecordingPurgeRequests, err = exportRecordingPurgeRequests(ctx, tx); err != nil {
			return err
		}
		if doc.RecordingWatched, err = exportRecordingWatched(ctx, tx); err != nil {
			return err
		}
		if doc.RecordingEncodePolicies, err = exportRecordingEncodePolicies(ctx, tx); err != nil {
			return err
		}
		if doc.MediaAssets, err = exportMediaAssets(ctx, tx, s.storeName); err != nil {
			return err
		}
		if doc.DropStats, err = exportDropStats(ctx, tx); err != nil {
			return err
		}
		if doc.DropPositions, err = exportDropPositions(ctx, tx); err != nil {
			return err
		}
		if doc.MediaAssetTSScans, err = exportMediaAssetTSScans(ctx, tx); err != nil {
			return err
		}
		if doc.RecordingChapterOwnerships, err = exportRecordingChapterOwnerships(ctx, tx); err != nil {
			return err
		}
		if doc.RecordingChapterSpans, err = exportRecordingChapterSpans(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("exporting catalog: %w", err)
	}
	return doc, nil
}

// exportRules は rules とその従属表を読み、文書の catalogdoc.Rule に組み立てる。
func exportRules(ctx context.Context, q rowQuerier) ([]catalogdoc.Rule, error) {
	rules, err := queryRows(ctx, q, "rules", `
 SELECT id, name, description, enabled, priority, is_free, duration_min_ms, duration_max_ms,
        period_start_at, period_end_at, dedupe_enabled, dedupe_threshold, dedupe_window,
        keep_original, encode_profiles, filename_template, metadata, created_at, updated_at
 FROM rules ORDER BY id`, scanRuleRow)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]int, len(rules))
	for i := range rules {
		byID[rules[i].ID] = i
	}
	// 子表は rule_id で親に付ける。親の順序は id 順のまま保つ。親の無い子行は FK により無い。
	// 子表の順序は ORDER BY で決まり、読んだ順に親へ追加する。
	if err := scanRuleChildren(ctx, q, "rule text matches", `
 SELECT rule_id, seq, target, mode, value, case_sensitive, negate FROM rule_text_matches ORDER BY rule_id, seq`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var m catalogdoc.RuleTextMatch
			err := rows.Scan(&ruleID, &m.Seq, &m.Target, &m.Mode, &m.Value, &m.CaseSensitive, &m.Negate)
			return ruleID, func(r *catalogdoc.Rule) { r.TextMatches = append(r.TextMatches, m) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	if err := scanRuleChildren(ctx, q, "rule services", `
 SELECT rule_id, network_id, service_id FROM rule_services ORDER BY rule_id, network_id, service_id`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var sv catalogdoc.RuleService
			err := rows.Scan(&ruleID, &sv.NetworkID, &sv.ServiceID)
			return ruleID, func(r *catalogdoc.Rule) { r.Services = append(r.Services, sv) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	if err := scanRuleChildren(ctx, q, "rule channel types", `
 SELECT rule_id, channel_type FROM rule_channel_types ORDER BY rule_id, channel_type`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var ct string
			err := rows.Scan(&ruleID, &ct)
			return ruleID, func(r *catalogdoc.Rule) { r.ChannelTypes = append(r.ChannelTypes, ct) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	if err := scanRuleChildren(ctx, q, "rule genres", `
 SELECT rule_id, genre_lv1 FROM rule_genres ORDER BY rule_id, genre_lv1`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var g int16
			err := rows.Scan(&ruleID, &g)
			return ruleID, func(r *catalogdoc.Rule) { r.Genres = append(r.Genres, g) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	if err := scanRuleChildren(ctx, q, "rule times", `
 SELECT rule_id, seq, weekdays, start_sec, end_sec FROM rule_times ORDER BY rule_id, seq`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var t catalogdoc.RuleTime
			err := rows.Scan(&ruleID, &t.Seq, &t.Weekdays, &t.StartSec, &t.EndSec)
			return ruleID, func(r *catalogdoc.Rule) { r.Times = append(r.Times, t) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	if err := scanRuleChildren(ctx, q, "rule sites", `
 SELECT rule_id, site FROM rule_sites ORDER BY rule_id, site`,
		func(rows *sql.Rows) (int64, func(*catalogdoc.Rule), error) {
			var ruleID int64
			var site string
			err := rows.Scan(&ruleID, &site)
			return ruleID, func(r *catalogdoc.Rule) { r.Sites = append(r.Sites, site) }, err
		}, byID, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// scanRuleChildren は子表を 1 行ずつ読み、親（rules の添字で引く）へ付ける。
// scan は親の ID と、親へ付ける操作を返す。
func scanRuleChildren(ctx context.Context, q rowQuerier, entity, query string,
	scan func(*sql.Rows) (int64, func(*catalogdoc.Rule), error), byID map[int64]int, rules []catalogdoc.Rule) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("listing %s: %w", entity, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		ruleID, apply, err := scan(rows)
		if err != nil {
			return fmt.Errorf("scanning %s: %w", entity, err)
		}
		if i, ok := byID[ruleID]; ok {
			apply(&rules[i])
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", entity, err)
	}
	return nil
}

func scanRuleRow(rows *sql.Rows) (catalogdoc.Rule, error) {
	var (
		r                            catalogdoc.Rule
		isFree                       sql.Null[bool]
		durMin, durMax, dedupeWindow sql.Null[int64]
		periodStart, periodEnd       sql.Null[time.Time]
		dedupeThreshold              sql.Null[float64]
		profiles, metadata           []byte
	)
	err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled, &r.Priority, &isFree, &durMin, &durMax,
		&periodStart, &periodEnd, &r.DedupeEnabled, &dedupeThreshold, &dedupeWindow,
		&r.KeepOriginal, &profiles, &r.FilenameTemplate, &metadata, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return catalogdoc.Rule{}, err
	}
	r.IsFree = nullPtr(isFree)
	r.DurationMinMs = nullPtr(durMin)
	r.DurationMaxMs = nullPtr(durMax)
	r.PeriodStartAt = nullPtr(periodStart)
	r.PeriodEndAt = nullPtr(periodEnd)
	if dedupeThreshold.Valid {
		v := float32(dedupeThreshold.V)
		r.DedupeThreshold = &v
	}
	r.DedupeWindowSeconds = nullPtr(dedupeWindow)
	r.EncodeProfiles, err = decodeStrings(profiles)
	if err != nil {
		return catalogdoc.Rule{}, fmt.Errorf("decoding rule %d encode profiles: %w", r.ID, err)
	}
	r.Metadata = metadata
	return r, nil
}

func exportRuleImportKeys(ctx context.Context, q rowQuerier) ([]catalogdoc.RuleImportKey, error) {
	return queryRows(ctx, q, "rule import keys",
		`SELECT rule_id, source, external_id FROM rule_import_keys ORDER BY rule_id`,
		func(rows *sql.Rows) (catalogdoc.RuleImportKey, error) {
			var k catalogdoc.RuleImportKey
			err := rows.Scan(&k.RuleID, &k.Source, &k.ExternalID)
			return k, err
		})
}

func exportLabelRules(ctx context.Context, q rowQuerier) ([]catalogdoc.LabelRule, error) {
	return queryRows(ctx, q, "label_rules",
		"SELECT id, `key`, value, keyword, priority, created_at, updated_at FROM label_rules ORDER BY id",
		func(rows *sql.Rows) (catalogdoc.LabelRule, error) {
			var r catalogdoc.LabelRule
			err := rows.Scan(&r.ID, &r.Key, &r.Value, &r.Keyword, &r.Priority, &r.CreatedAt, &r.UpdatedAt)
			return r, err
		})
}

// exportProgramSnapshots は意図・上書きの FK 先の program_snapshots だけを読む（Postgres 版と同じ）。
func exportProgramSnapshots(ctx context.Context, q rowQuerier) ([]catalogdoc.ProgramSnapshot, error) {
	return queryRows(ctx, q, "program_snapshots", `
 SELECT s.site, s.program_id, s.title, s.start_at, s.duration_ms, s.network_id, s.service_id,
        s.channel_type, s.channel, s.event_id, s.service_name, s.updated_at
 FROM program_snapshots s
 WHERE EXISTS (SELECT 1 FROM program_intents i WHERE i.site = s.site AND i.program_id = s.program_id)
    OR EXISTS (SELECT 1 FROM program_overrides o WHERE o.site = s.site AND o.program_id = s.program_id)
 ORDER BY s.site, s.program_id`,
		func(rows *sql.Rows) (catalogdoc.ProgramSnapshot, error) {
			var p catalogdoc.ProgramSnapshot
			err := rows.Scan(&p.Site, &p.ProgramID, &p.Title, &p.StartAt, &p.DurationMs, &p.NetworkID, &p.ServiceID,
				&p.ChannelType, &p.Channel, &p.EventID, &p.ServiceName, &p.UpdatedAt)
			return p, err
		})
}

func exportProgramIntents(ctx context.Context, q rowQuerier) ([]catalogdoc.ProgramIntent, error) {
	return queryRows(ctx, q, "program_intents",
		"SELECT site, program_id, action, created_at, updated_at FROM program_intents ORDER BY site, program_id",
		func(rows *sql.Rows) (catalogdoc.ProgramIntent, error) {
			var i catalogdoc.ProgramIntent
			err := rows.Scan(&i.Site, &i.ProgramID, &i.Action, &i.CreatedAt, &i.UpdatedAt)
			return i, err
		})
}

func exportProgramOverrides(ctx context.Context, q rowQuerier) ([]catalogdoc.ProgramOverride, error) {
	return queryRows(ctx, q, "program_overrides",
		"SELECT site, program_id, overrides, created_at, updated_at FROM program_overrides ORDER BY site, program_id",
		func(rows *sql.Rows) (catalogdoc.ProgramOverride, error) {
			var o catalogdoc.ProgramOverride
			var raw []byte
			err := rows.Scan(&o.Site, &o.ProgramID, &raw, &o.CreatedAt, &o.UpdatedAt)
			o.Overrides = raw
			return o, err
		})
}

func exportRecordings(ctx context.Context, q rowQuerier) ([]catalogdoc.Recording, error) {
	return queryRows(ctx, q, "recordings", `
 SELECT id, rule_id, source, site, network_id, service_id, event_id, service_name, channel_type, channel,
        title, description, extended, genres, is_free, program_start_at, program_duration_ms, status,
        started_at, ended_at, quality_events, failure_reported, deleted_at, superseded_at, purged_at,
        created_at, updated_at
 FROM recordings ORDER BY id`,
		func(rows *sql.Rows) (catalogdoc.Recording, error) {
			var r catalogdoc.Recording
			var ruleID sql.Null[int64]
			var startedAt, endedAt, deletedAt, supersededAt, purgedAt sql.Null[time.Time]
			var description sql.Null[string]
			var extended, genres, qualityEvents []byte
			var failureReported bool
			err := rows.Scan(&r.ID, &ruleID, &r.Source, &r.Site, &r.NetworkID, &r.ServiceID, &r.EventID,
				&r.ServiceName, &r.ChannelType, &r.Channel, &r.Title, &description, &extended, &genres,
				&r.IsFree, &r.ProgramStartAt, &r.ProgramDurationMs, &r.Status, &startedAt, &endedAt,
				&qualityEvents, &failureReported, &deletedAt, &supersededAt, &purgedAt,
				&r.CreatedAt, &r.UpdatedAt)
			if err != nil {
				return catalogdoc.Recording{}, err
			}
			r.RuleID = nullPtr(ruleID)
			r.Description = nullPtr(description)
			r.Extended = extended
			r.Genres = genres
			r.QualityEvents = qualityEvents
			r.FailureReported = &failureReported
			r.StartedAt = nullPtr(startedAt)
			r.EndedAt = nullPtr(endedAt)
			r.DeletedAt = nullPtr(deletedAt)
			r.SupersededAt = nullPtr(supersededAt)
			r.PurgedAt = nullPtr(purgedAt)
			return r, nil
		})
}

func exportRecordingPurgeRequests(ctx context.Context, q rowQuerier) ([]catalogdoc.RecordingPurgeRequest, error) {
	return queryRows(ctx, q, "recording_purge_requests",
		"SELECT recording_id, requested_at FROM recording_purge_requests ORDER BY recording_id",
		func(rows *sql.Rows) (catalogdoc.RecordingPurgeRequest, error) {
			var p catalogdoc.RecordingPurgeRequest
			err := rows.Scan(&p.RecordingID, &p.RequestedAt)
			return p, err
		})
}

func exportRecordingWatched(ctx context.Context, q rowQuerier) ([]catalogdoc.RecordingWatched, error) {
	return queryRows(ctx, q, "recording_watched",
		"SELECT recording_id, watched_at FROM recording_watched ORDER BY recording_id",
		func(rows *sql.Rows) (catalogdoc.RecordingWatched, error) {
			var w catalogdoc.RecordingWatched
			err := rows.Scan(&w.RecordingID, &w.WatchedAt)
			return w, err
		})
}

func exportRecordingEncodePolicies(ctx context.Context, q rowQuerier) ([]catalogdoc.RecordingEncodePolicy, error) {
	return queryRows(ctx, q, "recording_processing_policy", `
 SELECT recording_id, keep_original, encode_profiles, created_at, updated_at, cm_detect
 FROM recording_processing_policy ORDER BY recording_id`,
		func(rows *sql.Rows) (catalogdoc.RecordingEncodePolicy, error) {
			var p catalogdoc.RecordingEncodePolicy
			var profiles []byte
			if err := rows.Scan(&p.RecordingID, &p.KeepOriginal, &profiles, &p.CreatedAt, &p.UpdatedAt, &p.CMDetect); err != nil {
				return p, err
			}
			var err error
			p.EncodeProfiles, err = decodeStrings(profiles)
			return p, err
		})
}

// exportMediaAssets は media_assets を、指定した置き場の物理コピーと結んで文書の型付き行に変換する。
func exportMediaAssets(ctx context.Context, q rowQuerier, storeName string) ([]catalogdoc.MediaAsset, error) {
	return queryRows(ctx, q, "media_assets", `
 SELECT a.id, a.recording_id, a.kind, a.profile, copy.rel_path, a.size_bytes, copy.state, copy.deleted_at,
        copy.created_at, a.updated_at, a.derived_generation, a.sha256, copy.store, copy.generation
 FROM media_assets a
 JOIN media_asset_copies copy ON copy.media_asset_id = a.id AND copy.store = ?
 ORDER BY a.id, copy.id`,
		func(rows *sql.Rows) (catalogdoc.MediaAsset, error) {
			var a catalogdoc.MediaAsset
			var profile sql.Null[string]
			var deletedAt sql.Null[time.Time]
			var derivedGeneration, generation int64
			var sha []byte
			err := rows.Scan(&a.ID, &a.RecordingID, &a.Kind, &profile, &a.RelPath, &a.SizeBytes, &a.State,
				&deletedAt, &a.CreatedAt, &a.UpdatedAt, &derivedGeneration, &sha, &a.Store, &generation)
			if err != nil {
				return catalogdoc.MediaAsset{}, err
			}
			a.Profile = nullPtr(profile)
			a.DeletedAt = nullPtr(deletedAt)
			if a.Kind != "original" {
				a.DerivedGeneration = &derivedGeneration
			}
			a.Generation = &generation
			if len(sha) != 0 {
				v := hex.EncodeToString(sha)
				a.SHA256 = &v
			}
			return a, nil
		}, storeName)
}

func exportDropStats(ctx context.Context, q rowQuerier) ([]catalogdoc.DropStat, error) {
	return queryRows(ctx, q, "drop_stats", `
 SELECT media_asset_id, pid, packets, drops, errors, scrambled, pid_type
 FROM drop_stats ORDER BY media_asset_id, pid`,
		func(rows *sql.Rows) (catalogdoc.DropStat, error) {
			var d catalogdoc.DropStat
			var pidType sql.Null[string]
			err := rows.Scan(&d.MediaAssetID, &d.Pid, &d.Packets, &d.Drops, &d.Errors, &d.Scrambled, &pidType)
			d.PidType = nullPtr(pidType)
			return d, err
		})
}

func exportDropPositions(ctx context.Context, q rowQuerier) ([]catalogdoc.DropPosition, error) {
	return queryRows(ctx, q, "drop_positions",
		"SELECT media_asset_id, byte_offset, pid, elapsed_ms FROM drop_positions ORDER BY media_asset_id, byte_offset",
		func(rows *sql.Rows) (catalogdoc.DropPosition, error) {
			var p catalogdoc.DropPosition
			var elapsed sql.Null[int64]
			err := rows.Scan(&p.MediaAssetID, &p.ByteOffset, &p.Pid, &elapsed)
			p.ElapsedMs = nullPtr(elapsed)
			return p, err
		})
}

// exportMediaAssetTSScans は現在の内容のリビジョンと一致する計測だけを読む（Postgres 版の JOIN と同じ）。
func exportMediaAssetTSScans(ctx context.Context, q rowQuerier) ([]catalogdoc.MediaAssetTSScan, error) {
	return queryRows(ctx, q, "media_asset_ts_scans", `
 SELECT s.media_asset_id, s.scanned_size_bytes
 FROM media_asset_ts_scans s
 JOIN media_assets a ON a.id = s.media_asset_id AND a.content_revision = s.content_revision
 ORDER BY s.media_asset_id`,
		func(rows *sql.Rows) (catalogdoc.MediaAssetTSScan, error) {
			var s catalogdoc.MediaAssetTSScan
			err := rows.Scan(&s.MediaAssetID, &s.ScannedSizeBytes)
			return s, err
		})
}

func exportRecordingChapterOwnerships(ctx context.Context, q rowQuerier) ([]catalogdoc.RecordingChapterOwnership, error) {
	return queryRows(ctx, q, "recording_chapter_ownership",
		"SELECT recording_id, adopted_at FROM recording_chapter_ownership ORDER BY recording_id",
		func(rows *sql.Rows) (catalogdoc.RecordingChapterOwnership, error) {
			var o catalogdoc.RecordingChapterOwnership
			err := rows.Scan(&o.RecordingID, &o.AdoptedAt)
			return o, err
		})
}

func exportRecordingChapterSpans(ctx context.Context, q rowQuerier) ([]catalogdoc.RecordingChapterSpan, error) {
	return queryRows(ctx, q, "recording_chapter_spans",
		"SELECT recording_id, start_ms, end_ms, label, cut FROM recording_chapter_spans ORDER BY recording_id, start_ms, end_ms",
		func(rows *sql.Rows) (catalogdoc.RecordingChapterSpan, error) {
			var s catalogdoc.RecordingChapterSpan
			var label sql.Null[string]
			err := rows.Scan(&s.RecordingID, &s.StartMs, &s.EndMs, &label, &s.Cut)
			s.Label = nullPtr(label)
			return s, err
		})
}

// nullPtr は NULL を nil、値を値のポインタにする。
func nullPtr[T any](v sql.Null[T]) *T {
	if !v.Valid {
		return nil
	}
	value := v.V
	return &value
}

// decodeStrings は JSON 配列の文字列を読む。配列が無ければ空の非 nil を返す（Postgres 版の nonNilStrings と同じ）。
func decodeStrings(raw []byte) ([]string, error) {
	out := make([]string, 0)
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make([]string, 0)
	}
	return out, nil
}
