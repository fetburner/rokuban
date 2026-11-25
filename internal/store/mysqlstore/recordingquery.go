package mysqlstore

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/store"
)

// RecordingQueries は store.RecordingQuery の MySQL 実装である。
// storeName は読み出す置き場。空なら全置き場の active コピーを見る（API プロセス用）。
type RecordingQueries struct {
	db        *sql.DB
	storeName string
}

var _ store.RecordingQuery = (*RecordingQueries)(nil)

// NewRecordingQueries は録画の読み取りを返す。storeName が空なら全置き場を見る。
func NewRecordingQueries(db *sql.DB, storeName string) *RecordingQueries {
	return &RecordingQueries{db: db, storeName: storeName}
}

// recordingVisibleSQL は録画 API と録画 HLS が共有する削除状態の述語（r は recordings の別名）。
const recordingVisibleSQL = `r.deleted_at IS NULL AND r.purged_at IS NULL AND r.superseded_at IS NULL`

// recordingSQL は読み取り SQL を先頭から順に組む。プレースホルダ ? の並びは
// 文字列の並びと同じでなければならないので、引数は書いた位置で arg が積む。
// 文の途中で組んだ断片を後から前に書いてはならない。
type recordingSQL struct {
	storeName string
	text      strings.Builder
	args      []any
}

// w は断片を文の末尾へ書く。断片の中の arg 呼び出しは、評価された順（左から右）で引数に積まれる。
func (q *recordingSQL) w(parts ...string) {
	for _, p := range parts {
		q.text.WriteString(p)
	}
}

// arg は値を引数に積み、プレースホルダを返す。
func (q *recordingSQL) arg(v any) string {
	q.args = append(q.args, v)
	return "?"
}

// storeClause は col が読み出す置き場に一致する述語を返す。置き場が空なら全置き場（TRUE）。
func (q *recordingSQL) storeClause(col string) string {
	if q.storeName == "" {
		return "TRUE"
	}
	return col + " = " + q.arg(q.storeName)
}

// activeCopy は asset（media_assets の別名）に、読み出す置き場の active コピーが 1 つ以上あるかを返す。
func (q *recordingSQL) activeCopy(asset string) string {
	return "EXISTS (SELECT 1 FROM active_media_asset_copies c WHERE c.media_asset_id = " + asset + ".id AND " + q.storeClause("c.store") + ")"
}

// preferredOriginalStore は録画 recording（別名）の原本のうち、読み出す置き場の active コピーを
// 置き場名順で先頭の 1 件だけ返すスカラ副問い合わせ。Postgres の preferred_active_media_asset_copy を写す。
func (q *recordingSQL) preferredOriginalStore(recording string) string {
	return "(SELECT c.store FROM media_assets ao JOIN active_media_asset_copies c ON c.media_asset_id = ao.id" +
		" WHERE ao.recording_id = " + recording + ".id AND ao.kind = 'original' AND " + q.storeClause("c.store") +
		" ORDER BY c.store LIMIT 1)"
}

// inList は値のプレースホルダの並び（"?, ?"）を返す。値は引数に積む。
func inList[T any](q *recordingSQL, values []T) string {
	marks := make([]string, len(values))
	for i, v := range values {
		marks[i] = q.arg(v)
	}
	return strings.Join(marks, ", ")
}

// writeRecordingColumns は一覧と単体取得が共有する SELECT リストを書く。順序は scanRecordingQueryRow と一致させる。
// 一覧の要素と単体取得を同じ形に保つため、両方がこの 1 関数を通る。
func writeRecordingColumns(q *recordingSQL) {
	q.w(`
r.id, r.site, r.rule_id, r.source, r.service_name, r.channel_type, r.channel,
r.network_id, r.service_id, r.event_id, r.title, r.description,
r.program_start_at, r.program_duration_ms, r.status,
r.started_at, r.ended_at, r.quality_events, r.deleted_at, r.created_at,
a.size_bytes AS original_size_bytes,
CASE WHEN `+recordingVisibleSQL+` THEN COALESCE(
    (SELECT COALESCE(ao.ingest_generation, 0) FROM media_assets ao
     WHERE ao.recording_id = r.id AND ao.kind = 'original' AND `, q.activeCopy("ao"), `),
    ip.temp_generation) END AS hls_generation,
CASE WHEN `+recordingVisibleSQL+` THEN COALESCE(`, q.preferredOriginalStore("r"), `, NULLIF(ip.temp_store, '')) END AS hls_store,
`, q.preferredOriginalStore("r"), ` AS original_store,
COALESCE(d.packets, 0) AS drop_packets,
COALESCE(d.drops, 0) AS drop_drops,
COALESCE(d.errors, 0) AS drop_errors,
COALESCE(d.scrambled, 0) AS drop_scrambled,
COALESCE(p.keep_original, 'always') AS keep_original,
COALESCE(p.encode_profiles, JSON_ARRAY()) AS encode_profiles,
(SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('profile', ea.profile, 'state', ea.state)), JSON_ARRAY())
 FROM recording_encode_attempts ea WHERE ea.recording_id = r.id) AS encode_attempts,
EXISTS (SELECT 1 FROM media_assets o WHERE o.recording_id = r.id AND o.kind = 'original') AS has_original_asset,
EXISTS (SELECT 1 FROM current_ts_scanned_originals ts
        JOIN media_assets sa ON sa.id = ts.media_asset_id WHERE sa.recording_id = r.id) AS has_measured_drop_summary,
EXISTS (SELECT 1 FROM record_sync rs WHERE rs.recording_id = r.id AND rs.status IN ('recording', 'finished')) AS has_ingestable_record,
EXISTS (SELECT 1 FROM record_sync rs WHERE rs.recording_id = r.id AND rs.status IN ('canceled', 'failed')) AS has_abnormally_ended_record,
ip.written_bytes AS ingest_written_bytes,
ip.expected_bytes AS ingest_expected_bytes,
ip.observed_at AS ingest_observed_at,
COALESCE(p.cm_detect, FALSE) AS cm_detect,
EXISTS (SELECT 1 FROM recording_cm_detections cd WHERE cd.recording_id = r.id) AS cm_detected,
COALESCE((SELECT cd.cm_ranges FROM recording_cm_detections cd WHERE cd.recording_id = r.id), JSON_ARRAY()) AS cm_ranges,
ca.state AS cm_attempt_state,
ca.stage AS cm_attempt_stage,
ca.error AS cm_attempt_error,
EXISTS (SELECT 1 FROM recording_chapter_ownership co WHERE co.recording_id = r.id) AS chapters_owned,
COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('startMs', s.start_ms, 'endMs', s.end_ms,
                                          'label', s.label, 'cut', CAST(IF(s.cut, 'true', 'false') AS JSON)))
          FROM recording_chapter_spans s WHERE s.recording_id = r.id), JSON_ARRAY()) AS chapter_spans,
(SELECT rsv.value FROM recording_series rsv WHERE rsv.recording_id = r.id) AS series,
r.series_key AS series_key,
pp.position_ms AS resume_position_ms,
pp.updated_at AS resume_position_updated_at,
(SELECT MAX(w.watched_at) FROM recording_watched w
 JOIN recordings event_recording ON event_recording.id = w.recording_id
 WHERE event_recording.network_id = r.network_id
   AND event_recording.service_id = r.service_id
   AND event_recording.program_start_at = r.program_start_at) AS watched_at,
`, `COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('profile', e.profile, 'sizeBytes', e.size_bytes,
                                             'keepRanges', (SELECT k.keep_ranges FROM media_asset_cuts k WHERE k.media_asset_id = e.id)))
    FROM media_assets e
    WHERE e.recording_id = r.id AND e.kind = 'encoded' AND e.profile IS NOT NULL AND `, q.activeCopy("e"), `
      AND NOT EXISTS (SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = e.id)), JSON_ARRAY()) AS available_encoded_assets`)
}

// writeRecordingJoins は一覧と単体取得が共有する FROM と JOIN を書く。
// 原本は削除途中のコピーも含めて読む（削除済みコピーだけを除く）。読み出す置き場の絞りは storeClause が担う。
func writeRecordingJoins(q *recordingSQL) {
	q.w(`
FROM recordings r
LEFT JOIN media_assets a ON a.recording_id = r.id AND a.kind = 'original' AND EXISTS (
    SELECT 1 FROM media_asset_copies copy
    WHERE copy.media_asset_id = a.id AND `, q.storeClause("copy.store"), ` AND copy.state <> 'deleted')
LEFT JOIN recording_processing_policy p ON p.recording_id = r.id
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN recording_ingest_progress ip ON ip.recording_id = r.id
LEFT JOIN recording_playback_positions pp ON pp.recording_id = r.id
LEFT JOIN LATERAL (
    SELECT SUM(ds.packets) AS packets, SUM(ds.drops) AS drops,
           SUM(ds.errors) AS errors, SUM(ds.scrambled) AS scrambled
    FROM drop_stats ds
    JOIN media_assets da ON da.id = ds.media_asset_id
    WHERE da.recording_id = r.id AND da.kind = 'original'
) d ON TRUE`)
}

// narrowSearchPairs は検索語の正規化（narrowEPGSearchRune）が変える文字の対応。SQL の REPLACE 式と
// 検索語の Go 側の写像を、同じ関数から 1 回だけ作る。
var narrowSearchPairs = buildNarrowSearchPairs()

func buildNarrowSearchPairs() [][2]string {
	var pairs [][2]string
	add := func(from, to rune) {
		for r := from; r <= to; r++ {
			if n := narrowEPGSearchRune(r); n != r {
				pairs = append(pairs, [2]string{string(r), string(n)})
			}
		}
	}
	add('Ａ', 'Ｚ')
	add('ａ', 'ｚ')
	add('０', '９')
	add('\u3000', '\u3000')
	return pairs
}

// normalizeSearchTextSQL は検索語の正規化と同じ写像を col に当て、MySQL の LOWER で小文字にする SQL 式を返す。
// 文字列リテラルは narrowSearchPairs の 1 文字だけで、引用符を含まないので埋め込んでよい。
func normalizeSearchTextSQL(col string) string {
	expr := col
	for _, p := range narrowSearchPairs {
		expr = "REPLACE(" + expr + ", '" + p[0] + "', '" + p[1] + "')"
	}
	return "LOWER(" + expr + ")"
}

// keywordClause は col を正規化した値に、正規化した語が部分一致する述語を返す。
// 語は epgLikeEscaper で LIKE の特殊文字を文字どおりにし、LOWER(?) で小文字にする（compileEPGTextMatch と同じ形）。
func keywordClause(q *recordingSQL, col, value string) string {
	pattern := "%" + epgLikeEscaper.Replace(strings.Map(narrowEPGSearchRune, value)) + "%"
	return normalizeSearchTextSQL(col) + " COLLATE utf8mb4_0900_bin LIKE LOWER(" + q.arg(pattern) + ") ESCAPE '!'"
}

// recordingsFilterClauses は録画一覧と棚が共有する絞り込みを、録画 1 件（別名 r）の述語にする。
// 条件が無い軸は節を足さない。ページング・並び順・番組ハブ・続きから見るは録画一覧だけの軸なので含めない。
func recordingsFilterClauses(q *recordingSQL, f store.RecordingFilter) []string {
	var clauses []string
	if f.Q != "" {
		if f.QTarget == "title" {
			clauses = append(clauses, keywordClause(q, "r.title", f.Q))
		} else {
			clauses = append(clauses, "("+keywordClause(q, "r.title", f.Q)+" OR "+keywordClause(q, "r.description", f.Q)+")")
		}
	}
	if len(f.Genres) > 0 {
		levels := make([]string, len(f.Genres))
		for i, lv1 := range f.Genres {
			levels[i] = strconv.Itoa(int(lv1))
		}
		clauses = append(clauses, "JSON_OVERLAPS(r.genre_lv1, CAST("+q.arg("["+strings.Join(levels, ",")+"]")+" AS JSON))")
	}
	if len(f.ChannelTypes) > 0 {
		clauses = append(clauses, "r.channel_type IN ("+inList(q, f.ChannelTypes)+")")
	}
	if len(f.Sites) > 0 {
		clauses = append(clauses, "r.site IN ("+inList(q, f.Sites)+")")
	}
	if len(f.Services) > 0 {
		// 行値の IN。(network_id, service_id) の組で OR する（軸どうしの直積にはしない）。
		pairs := make([]string, len(f.Services))
		for i, service := range f.Services {
			pairs[i] = "(" + q.arg(service.NetworkID) + ", " + q.arg(service.ServiceID) + ")"
		}
		clauses = append(clauses, "(r.network_id, r.service_id) IN ("+strings.Join(pairs, ", ")+")")
	}
	if f.Status != "" {
		clauses = append(clauses, "r.status = "+q.arg(f.Status))
		// 通常一覧の status=failed だけは、置き換わった擬似 failed 行を除く（Postgres と同じ判断）。
		if !f.Trash && f.Status == "failed" {
			clauses = append(clauses, "r.superseded_at IS NULL")
		}
	}
	if f.Source != "" {
		clauses = append(clauses, "r.source = "+q.arg(f.Source))
	}
	if f.RuleID != nil {
		clauses = append(clauses, "r.rule_id = "+q.arg(*f.RuleID))
	}
	if f.From != nil {
		clauses = append(clauses, "r.program_start_at >= "+q.arg(*f.From))
	}
	if f.To != nil {
		clauses = append(clauses, "r.program_start_at < "+q.arg(*f.To))
	}
	return clauses
}

// recordingListClauses は録画一覧の WHERE の節を組む。
func recordingListClauses(q *recordingSQL, f store.RecordingFilter) []string {
	var clauses []string
	// trash の有無で基底述語を切り替える。通常一覧では deleted_at だけを絞り、superseded_at は履歴を保つため絞らない。
	if f.Trash {
		clauses = append(clauses, "r.deleted_at IS NOT NULL", "r.purged_at IS NULL")
	} else {
		clauses = append(clauses, "r.deleted_at IS NULL")
	}
	clauses = append(clauses, recordingsFilterClauses(q, f)...)
	if f.EncodeRecordingIDs != nil {
		if len(*f.EncodeRecordingIDs) == 0 {
			clauses = append(clauses, "FALSE")
		} else {
			clauses = append(clauses, "r.id IN ("+inList(q, *f.EncodeRecordingIDs)+")")
		}
	}
	// 番組ハブ。起点の実効シリーズと等しい録画に絞る。NULL 同士は等しくないので、起点が無ければ 0 件になる。
	if f.SeriesOf != nil {
		clauses = append(clauses,
			"(SELECT s.value FROM recording_series s WHERE s.recording_id = r.id) = "+
				"(SELECT s.value FROM recording_series s WHERE s.recording_id = "+q.arg(*f.SeriesOf)+")",
			"r.superseded_at IS NULL")
	}
	if f.ContinueWatching {
		clauses = append(clauses,
			"pp.recording_id IS NOT NULL",
			"r.deleted_at IS NULL",
			"r.purged_at IS NULL",
			"r.superseded_at IS NULL",
			"r.status IN ('recording', 'finished')",
			`NOT EXISTS (
        SELECT 1
        FROM recordings event_recording
        JOIN recording_watched w ON w.recording_id = event_recording.id
        WHERE event_recording.network_id = r.network_id
          AND event_recording.service_id = r.service_id
          AND event_recording.program_start_at = r.program_start_at
    )`)
	}
	// キーセットは (program_start_at, id) の複合で割る。同時刻の別チャンネルの録画で重複・欠落を防ぐ。
	if f.Before != nil {
		op := ">"
		if f.SortDesc {
			op = "<"
		}
		clauses = append(clauses, "(r.program_start_at, r.id) "+op+" ("+q.arg(*f.Before)+", "+q.arg(*f.BeforeID)+")")
	}
	return clauses
}

// List は録画の一覧を返す。
func (s *RecordingQueries) List(ctx context.Context, filter store.RecordingFilter) ([]store.RecordingQueryRow, error) {
	if (filter.Before == nil) != (filter.BeforeID == nil) {
		return nil, fmt.Errorf("before and beforeId must be given together")
	}
	q := &recordingSQL{storeName: s.storeName}
	q.w("SELECT ")
	writeRecordingColumns(q)
	writeRecordingJoins(q)
	clauses := recordingListClauses(q, filter)

	orderDir := "ASC"
	if filter.SortDesc {
		orderDir = "DESC"
	}
	orderBy := "r.program_start_at " + orderDir + ", r.id " + orderDir
	if filter.ContinueWatching {
		orderBy = "pp.updated_at DESC, r.id DESC"
	}
	q.w(" WHERE ", strings.Join(clauses, " AND "), " ORDER BY ", orderBy, " LIMIT ", q.arg(filter.Limit))

	rows, err := s.db.QueryContext(ctx, q.text.String(), q.args...)
	if err != nil {
		return nil, fmt.Errorf("querying recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]store.RecordingQueryRow, 0, filter.Limit)
	for rows.Next() {
		fields, err := scanRecordingQueryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning recording row: %w", err)
		}
		result = append(result, fields)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recordings: %w", err)
	}
	return result, nil
}

// Get は録画の素の射影を返す。ごみ箱行も含み、purged の tombstone は返さない。
func (s *RecordingQueries) Get(ctx context.Context, recordingID int64) (store.RecordingQueryRow, error) {
	q := &recordingSQL{storeName: s.storeName}
	q.w("SELECT ")
	writeRecordingColumns(q)
	writeRecordingJoins(q)
	q.w(" WHERE r.id = ", q.arg(recordingID), " AND r.purged_at IS NULL")

	fields, err := scanRecordingQueryRow(s.db.QueryRowContext(ctx, q.text.String(), q.args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.RecordingQueryRow{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.RecordingQueryRow{}, fmt.Errorf("querying recording %d: %w", recordingID, err)
	}
	return fields, nil
}

// Shelves は実効シリーズごとの棚集計を返す。
//
// 代表は番組開始時刻の新しい順で先頭の 1 件（同時刻なら id の大きい方）。Postgres の array_agg(... ORDER BY)[1] と同じ。
// 未視聴件数は再生可能な録画の放送イベントを、視聴済みの印（絞り込みの外も含む）と突き合わせて数える。
func (s *RecordingQueries) Shelves(ctx context.Context, filter store.RecordingFilter) ([]store.RecordingShelf, error) {
	q := &recordingSQL{storeName: s.storeName}
	q.w(`WITH playable_assets AS (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE ma.kind IN ('original', 'encoded') AND EXISTS (
        SELECT 1 FROM active_media_asset_copies c WHERE c.media_asset_id = ma.id AND `, q.storeClause("c.store"), `
    )
),
watched_events AS (
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_watched w ON w.recording_id = r.id
),
live AS (
    SELECT r.id, r.title, r.program_start_at, r.network_id, r.service_id, rs.value,
           pa.recording_id AS playable_recording_id,
           we.network_id AS watched_network_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    LEFT JOIN watched_events we
      ON we.network_id = r.network_id
     AND we.service_id = r.service_id
     AND we.program_start_at = r.program_start_at
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL`)
	for _, clause := range recordingsFilterClauses(q, filter) {
		q.w(" AND ", clause)
	}
	q.w(`
),
ranked AS (
    SELECT l.id, l.title, l.program_start_at, l.value,
           ROW_NUMBER() OVER (PARTITION BY l.value ORDER BY l.program_start_at DESC, l.id DESC) AS rn
    FROM live l
),
grouped AS (
    SELECT l.value,
           COUNT(*) AS recording_count,
           COUNT(l.playable_recording_id) AS playable_count,
           COUNT(DISTINCT
               CASE WHEN l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL THEN l.network_id END,
               CASE WHEN l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL THEN l.service_id END,
               CASE WHEN l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL THEN l.program_start_at END
           ) AS unwatched_count,
           MAX(l.program_start_at) AS latest_start_at
    FROM live l
    GROUP BY l.value
)
SELECT g.value, rep.title, g.recording_count, g.playable_count, g.unwatched_count, g.latest_start_at, rep.id
FROM grouped g
JOIN ranked rep ON rep.rn = 1 AND rep.value <=> g.value
ORDER BY g.recording_count DESC, (g.value IS NULL), g.value`)

	rows, err := s.db.QueryContext(ctx, q.text.String(), q.args...)
	if err != nil {
		return nil, fmt.Errorf("listing recording shelves: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]store.RecordingShelf, 0)
	for rows.Next() {
		var shelf store.RecordingShelf
		if err := rows.Scan(&shelf.Value, &shelf.Title, &shelf.Count, &shelf.PlayableCount,
			&shelf.UnwatchedCount, &shelf.LatestStartAt, &shelf.RepresentativeID); err != nil {
			return nil, fmt.Errorf("scanning recording shelf: %w", err)
		}
		result = append(result, shelf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recording shelves: %w", err)
	}
	return result, nil
}

type recordingRowScanner interface {
	Scan(dest ...any) error
}

// scanRecordingQueryRow は writeRecordingColumns の順に 1 行を読み、配列の並びを決める。
// JSON_ARRAYAGG は ORDER BY を受けないので、並びは読み出し後に Go で決める（Postgres の ORDER BY と同じ順）。
func scanRecordingQueryRow(row recordingRowScanner) (store.RecordingQueryRow, error) {
	var (
		fields         store.RecordingQueryRow
		encodeProfiles []byte
	)
	err := row.Scan(
		&fields.ID, &fields.Site, &fields.RuleID, &fields.Source, &fields.ServiceName, &fields.ChannelType, &fields.Channel,
		&fields.NetworkID, &fields.ServiceID, &fields.EventID, &fields.Title, &fields.Description,
		&fields.ProgramStartAt, &fields.ProgramDurationMs, &fields.Status,
		&fields.StartedAt, &fields.EndedAt, &fields.QualityEvents, &fields.DeletedAt, &fields.CreatedAt,
		&fields.OriginalSizeBytes, &fields.HLSGeneration, &fields.HLSStore, &fields.OriginalStore,
		&fields.DropPackets, &fields.DropDrops, &fields.DropErrors, &fields.DropScrambled,
		&fields.KeepOriginal, &encodeProfiles, &fields.EncodeAttempts,
		&fields.HasOriginalAsset, &fields.HasMeasuredDropSummary,
		&fields.HasIngestableRecord, &fields.HasAbnormallyEndedRecord,
		&fields.IngestWrittenBytes, &fields.IngestExpectedBytes, &fields.IngestObservedAt,
		&fields.CMDetect, &fields.CMDetected, &fields.CMRanges, &fields.CMAttemptState,
		&fields.CMAttemptStage, &fields.CMAttemptError,
		&fields.ChaptersOwned, &fields.ChapterSpans,
		&fields.Series, &fields.SeriesKey,
		&fields.ResumePositionMs, &fields.ResumePositionUpdatedAt, &fields.WatchedAt,
		&fields.AvailableEncodedAssets,
	)
	if err != nil {
		return store.RecordingQueryRow{}, err
	}
	fields.EncodeProfiles = []string{}
	if err := json.Unmarshal(encodeProfiles, &fields.EncodeProfiles); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("decoding encode profiles: %w", err)
	}
	if fields.EncodeAttempts, err = sortedJSONArray(fields.EncodeAttempts, profileOf); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("ordering encode attempts: %w", err)
	}
	if fields.CMRanges, err = sortedJSONArray(fields.CMRanges, startMsOf); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("ordering cm ranges: %w", err)
	}
	if fields.ChapterSpans, err = sortedJSONArray(fields.ChapterSpans, startMsOf); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("ordering chapter spans: %w", err)
	}
	if fields.AvailableEncodedAssets, err = sortedJSONArray(fields.AvailableEncodedAssets, profileOf); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("ordering available encoded assets: %w", err)
	}
	if fields.AvailableEncodedAssets, err = sortAssetKeepRanges(fields.AvailableEncodedAssets); err != nil {
		return store.RecordingQueryRow{}, fmt.Errorf("ordering keep ranges: %w", err)
	}
	return fields, nil
}

// sortedJSONArray は JSON 配列の要素を key の昇順に並べ直す。同じ key の要素は元の順を保つ。
func sortedJSONArray[K cmp.Ordered](raw json.RawMessage, key func(json.RawMessage) (K, error)) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decoding JSON array: %w", err)
	}
	type keyed struct {
		key K
		raw json.RawMessage
	}
	pairs := make([]keyed, len(items))
	for i, item := range items {
		k, err := key(item)
		if err != nil {
			return nil, err
		}
		pairs[i] = keyed{key: k, raw: item}
	}
	slices.SortStableFunc(pairs, func(a, b keyed) int { return cmp.Compare(a.key, b.key) })
	sorted := make([]json.RawMessage, len(pairs))
	for i, p := range pairs {
		sorted[i] = p.raw
	}
	return json.Marshal(sorted)
}

// sortAssetKeepRanges は利用可能な版の keepRanges を startMs の昇順に並べ直す。null の keepRanges は null のまま。
// Postgres の keepRanges は ORDER BY lower(k) で並ぶので、読み手側で同じ順に揃える。
func sortAssetKeepRanges(raw json.RawMessage) (json.RawMessage, error) {
	var assets []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &assets); err != nil {
		return nil, fmt.Errorf("decoding available encoded assets: %w", err)
	}
	for _, asset := range assets {
		ranges := asset["keepRanges"]
		if len(ranges) == 0 || string(ranges) == "null" {
			continue
		}
		sorted, err := sortedJSONArray(ranges, startMsOf)
		if err != nil {
			return nil, fmt.Errorf("ordering keep ranges: %w", err)
		}
		asset["keepRanges"] = sorted
	}
	return json.Marshal(assets)
}

func profileOf(item json.RawMessage) (string, error) {
	var v struct {
		Profile string `json:"profile"`
	}
	err := json.Unmarshal(item, &v)
	return v.Profile, err
}

func startMsOf(item json.RawMessage) (int64, error) {
	var v struct {
		StartMs int64 `json:"startMs"`
	}
	err := json.Unmarshal(item, &v)
	return v.StartMs, err
}
