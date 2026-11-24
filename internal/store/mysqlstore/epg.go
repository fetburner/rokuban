package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store"
)

// EPG は番組表の同期・読み取り・検索を MySQL で実装する。
type EPG struct {
	db *sql.DB
}

// NewEPG は MySQL を使う EPG store を作る。
func NewEPG(db *sql.DB) *EPG {
	return &EPG{db: db}
}

// SweepMark は EPG 同期の基準となる DB 時刻を返す。
func (s *EPG) SweepMark(ctx context.Context) (time.Time, error) {
	var mark time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6)").Scan(&mark); err != nil {
		return time.Time{}, fmt.Errorf("getting EPG sweep mark: %w", err)
	}
	return mark, nil
}

const upsertEPGServices = `
INSERT INTO epg_services (
    site, network_id, service_id, type, logo_id, remote_control_key_id,
    name, channel_type, channel, has_logo_data, observed_at
) VALUES `

const updateEPGServices = `
AS incoming ON DUPLICATE KEY UPDATE
    type = incoming.type, logo_id = incoming.logo_id,
    remote_control_key_id = incoming.remote_control_key_id, name = incoming.name,
    channel_type = incoming.channel_type, channel = incoming.channel,
    has_logo_data = incoming.has_logo_data, observed_at = incoming.observed_at`

// UpsertServices はサービス射影の 1 バッチを書き込む。
func (s *EPG) UpsertServices(ctx context.Context, services []store.EPGServiceUpsert) error {
	return upsertEPGBatch(ctx, s.db, "services", services, 11, upsertEPGServices, updateEPGServices,
		func(args []any, p store.EPGServiceUpsert, mark time.Time) ([]any, error) {
			return append(args, p.Site, p.NetworkID, p.ServiceID, p.Type, p.LogoID, p.RemoteControlKeyID,
				p.Name, p.ChannelType, p.Channel, p.HasLogoData, mark), nil
		})
}

const upsertEPGPrograms = `
INSERT INTO epg_programs (
    site, program_id, network_id, service_id, event_id, start_at, duration_ms,
    end_at, is_free, name, description, extended, genres, video, audios, genre_lv1, series_key, observed_at
) VALUES `

const updateEPGPrograms = `
AS incoming ON DUPLICATE KEY UPDATE
    network_id = incoming.network_id, service_id = incoming.service_id, event_id = incoming.event_id,
    start_at = incoming.start_at, duration_ms = incoming.duration_ms, end_at = incoming.end_at,
    is_free = incoming.is_free, name = incoming.name, description = incoming.description,
    extended = incoming.extended, genres = incoming.genres, video = incoming.video, audios = incoming.audios,
    genre_lv1 = incoming.genre_lv1, series_key = incoming.series_key, observed_at = incoming.observed_at`

// UpsertPrograms は番組射影の 1 バッチを書き込む。
func (s *EPG) UpsertPrograms(ctx context.Context, programs []store.EPGProgramUpsert) error {
	return upsertEPGBatch(ctx, s.db, "programs", programs, 18, upsertEPGPrograms, updateEPGPrograms,
		func(args []any, p store.EPGProgramUpsert, mark time.Time) ([]any, error) {
			var series any
			if key, ok := serieskey.SeriesKey(p.Name); ok {
				series = key
			}
			return append(args, p.Site, p.ProgramID, p.NetworkID, p.ServiceID, p.EventID, p.StartAt,
				p.DurationMs, p.EndAt, p.IsFree, p.Name, p.Description, nullableJSON(p.Extended),
				nullableJSON(p.Genres), nullableJSON(p.Video), nullableJSON(p.Audios), epgGenreLevels(p.Genres), series, mark), nil
		})
}

// upsertEPGBatch は文のパラメータ数を抑えつつ、バッチを同じ観測時刻の 1 tx で確定する。
// 両表で衝突しうる一意キーは主キーだけなので、行エイリアスの upsert を使う。
func upsertEPGBatch[T any](ctx context.Context, db *sql.DB, entity string, batch []T, columns int, insert, update string,
	rowArgs func([]any, T, time.Time) ([]any, error)) error {
	if len(batch) == 0 {
		return nil
	}
	err := runTx(ctx, db, func(tx *sql.Tx) error {
		var mark time.Time
		if err := tx.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6)").Scan(&mark); err != nil {
			return fmt.Errorf("getting EPG observation time: %w", err)
		}
		tuple := "(" + placeholders(columns) + ")"
		for chunk := range slices.Chunk(batch, 1000) {
			args := make([]any, 0, len(chunk)*columns)
			for _, row := range chunk {
				var err error
				args, err = rowArgs(args, row, mark)
				if err != nil {
					return err
				}
			}
			query := insert + strings.Repeat(tuple+", ", len(chunk)-1) + tuple + update
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upserting EPG %s: %w", entity, err)
	}
	return nil
}

func placeholders(count int) string {
	return strings.Repeat("?, ", count-1) + "?"
}

func nullableJSON(value json.RawMessage) any {
	if value == nil {
		return nil
	}
	return string(value)
}

// epgGenreLevels は詳細・一覧・検索が共用する、昇順で重複のない lv1 を作る。
func epgGenreLevels(genres json.RawMessage) string {
	var entries []json.RawMessage
	if err := json.Unmarshal(genres, &entries); err != nil {
		// 配列以外に lv1 は無い。不正な JSON は元の genres を書き込むときに DB が拒否する。
		return "[]"
	}
	var present [16]bool
	for _, entry := range entries {
		// キーは JSON と同じく大文字小文字を区別する。
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil {
			continue
		}
		level := fields["lv1"]
		if len(level) == 0 {
			continue
		}
		var text string
		switch {
		case level[0] == '"':
			// fields を読むときに JSON の構文は検査済み。
			_ = json.Unmarshal(level, &text)
		case level[0] == '-' || (level[0] >= '0' && level[0] <= '9'):
			text = genreIntegerText(string(level))
		default:
			continue
		}
		if text == "" || strings.ContainsFunc(text, func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		if value, err := strconv.ParseUint(text, 10, 4); err == nil {
			present[value] = true
		}
	}
	levels := make([]int16, 0, len(present))
	for level, found := range present {
		if found {
			levels = append(levels, int16(level))
		}
	}
	data, _ := json.Marshal(levels) // []int16 は常に JSON に変換できる。
	return string(data)
}

// genreIntegerText は genre_lv1_of と同様に、指数を展開してから小数部の有無を判定する。
func genreIntegerText(value string) string {
	mantissa, power, hasExponent := strings.Cut(strings.ToLower(value), "e")
	var exponent int
	if hasExponent {
		// 構文は検査済み。範囲外の指数は int の端に丸まり、以下の桁数判定で扱う。
		exponent, _ = strconv.Atoi(power)
	}
	mantissa, negative := strings.CutPrefix(mantissa, "-")
	whole, frac, _ := strings.Cut(mantissa, ".")
	if exponent < len(frac) {
		return ""
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return "0"
	}
	zeroes := exponent - len(frac)
	if negative || zeroes > 2-len(digits) {
		return ""
	}
	return digits + strings.Repeat("0", zeroes)
}

// DeleteStaleServices は今回観測しなかったサービス射影を削除する。
func (s *EPG) DeleteStaleServices(ctx context.Context, site string, mark time.Time) (int64, error) {
	return s.deleteRows(ctx, "stale EPG services", `
DELETE FROM epg_services WHERE site = ? AND observed_at < ?`, site, mark)
}

// DeleteStalePrograms は観測済みチャンネル群の古い番組射影を削除する。
func (s *EPG) DeleteStalePrograms(ctx context.Context, site string, mark time.Time, networkID int32, serviceIDs []int32) (int64, error) {
	if len(serviceIDs) == 0 {
		return 0, nil
	}
	args := []any{site, mark, networkID}
	for _, id := range serviceIDs {
		args = append(args, id)
	}
	return s.deleteRows(ctx, "stale EPG programs", `
DELETE FROM epg_programs WHERE site = ? AND observed_at < ? AND network_id = ?
  AND service_id IN (`+placeholders(len(serviceIDs))+`)`, args...)
}

// PrunePrograms は保持期間を過ぎた番組射影を削除する。
func (s *EPG) PrunePrograms(ctx context.Context, site string, endAt time.Time) (int64, error) {
	return s.deleteRows(ctx, "aired EPG programs", `
DELETE FROM epg_programs WHERE site = ? AND end_at < ?`, site, endAt)
}

func (s *EPG) deleteRows(ctx context.Context, entity, query string, args ...any) (int64, error) {
	var count int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("deleting %s: %w", entity, err)
	}
	return count, nil
}

// NotifyProgramsUpdated は番組表更新を購読者へ通知する。
func (s *EPG) NotifyProgramsUpdated(ctx context.Context) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := NotifyTopic(ctx, tx, "epg"); err != nil {
			return fmt.Errorf("notifying EPG programs updated: %w", err)
		}
		return nil
	})
}

// ListServices は site のサービス射影を返す。
func (s *EPG) ListServices(ctx context.Context, site string) ([]store.EPGService, error) {
	return queryEPGRows(ctx, s.db, "EPG services", `
SELECT s.site, s.network_id, s.service_id, s.name, s.channel_type, s.channel, s.remote_control_key_id, s.has_logo_data,
       EXISTS (SELECT 1 FROM epg_programs p WHERE p.site = s.site AND p.network_id = s.network_id AND p.service_id = s.service_id)
FROM epg_services s
WHERE s.site = ?
ORDER BY s.channel_type, s.remote_control_key_id, s.service_id`, func(rows *sql.Rows) (store.EPGService, error) {
		var p store.EPGService
		err := rows.Scan(&p.Site, &p.NetworkID, &p.ServiceID, &p.Name, &p.ChannelType, &p.Channel,
			&p.RemoteControlKeyID, &p.HasLogoData, &p.HasPrograms)
		return p, err
	}, site)
}

const epgProgramColumns = `p.site, p.program_id, p.network_id, p.service_id, p.event_id,
       p.start_at, p.duration_ms, p.end_at, p.is_free, p.name, p.description, p.genre_lv1`

const epgProgramWithIntent = `
FROM epg_programs p
LEFT JOIN program_intents i ON i.site = p.site AND i.program_id = p.program_id
`

// ListPrograms は時間窓と任意の NID/SID 集合で番組射影を絞る。
func (s *EPG) ListPrograms(ctx context.Context, filter store.EPGProgramListQuery) ([]store.EPGProgramListItem, error) {
	query := "SELECT " + epgProgramColumns + ", i.action, r.id" + epgProgramWithIntent + `
LEFT JOIN recordings r
  ON r.site = p.site AND r.network_id = p.network_id AND r.service_id = p.service_id
 AND r.event_id = p.event_id AND r.active_program_start_at = p.start_at
 AND r.status = 'recording'
WHERE p.site = ? AND p.start_at < ? AND p.end_at > ?`
	args := []any{filter.Site, filter.WindowEnd, filter.WindowStart}
	if len(filter.Services) != 0 {
		pairs := make([]string, 0, len(filter.Services))
		for _, service := range filter.Services {
			pairs = append(pairs, "(?, ?)")
			args = append(args, service.NetworkID, service.ServiceID)
		}
		query += " AND (p.network_id, p.service_id) IN (" + strings.Join(pairs, ", ") + ")"
	}
	query += " ORDER BY p.start_at, p.network_id, p.service_id"
	return queryEPGRows(ctx, s.db, "EPG programs", query, func(rows *sql.Rows) (store.EPGProgramListItem, error) {
		var p store.EPGProgramListItem
		var genres []byte
		if err := rows.Scan(&p.Site, &p.ProgramID, &p.NetworkID, &p.ServiceID, &p.EventID,
			&p.StartAt, &p.DurationMs, &p.EndAt, &p.IsFree, &p.Name, &p.Description, &genres,
			&p.IntentAction, &p.RecordingID); err != nil {
			return p, err
		}
		if err := json.Unmarshal(genres, &p.GenreLv1); err != nil {
			return p, fmt.Errorf("decoding EPG program genre levels: %w", err)
		}
		return p, nil
	}, args...)
}

// GetProgram は site の programID の詳細射影を返す。
func (s *EPG) GetProgram(ctx context.Context, site string, programID int64) (store.EPGProgramDetail, error) {
	query := "SELECT " + epgProgramColumns + ", p.extended, p.genres, p.video, p.audios, i.action" +
		epgProgramWithIntent + "WHERE p.site = ? AND p.program_id = ?"
	var detail store.EPGProgramDetail
	p := &detail.Program
	var genres, extended, sourceGenres, video, audios []byte
	err := s.db.QueryRowContext(ctx, query, site, programID).Scan(&p.Site, &p.ProgramID, &p.NetworkID, &p.ServiceID, &p.EventID,
		&p.StartAt, &p.DurationMs, &p.EndAt, &p.IsFree, &p.Name, &p.Description, &genres,
		&extended, &sourceGenres, &video, &audios, &detail.IntentAction)
	if errors.Is(err, sql.ErrNoRows) {
		return store.EPGProgramDetail{}, fmt.Errorf("program %d for site %q: %w", programID, site, store.ErrNotFound)
	}
	if err != nil {
		return store.EPGProgramDetail{}, fmt.Errorf("getting EPG program %d for site %q: %w", programID, site, err)
	}
	if err := json.Unmarshal(genres, &p.GenreLv1); err != nil {
		return store.EPGProgramDetail{}, fmt.Errorf("reading EPG program %d genre levels: %w", programID, err)
	}
	p.Extended, p.Genres, p.Video, p.Audios = extended, sourceGenres, video, audios
	return detail, nil
}

// ListUpcomingProgramsBySeries は録画の実効シリーズに対応する今後の番組を返す。
func (s *EPG) ListUpcomingProgramsBySeries(ctx context.Context, recordingID int64) ([]store.ProgramMatch, error) {
	return queryEPGRows(ctx, s.db, "upcoming EPG programs", "SELECT "+programMatchColumns+`
FROM epg_program_series s
JOIN epg_programs p ON p.site = s.site AND p.program_id = s.program_id
WHERE s.value = (SELECT rs.value FROM recording_series rs WHERE rs.recording_id = ?)
  AND p.start_at > CURRENT_TIMESTAMP(6)
ORDER BY p.start_at, p.site, p.program_id`, scanProgramMatch, recordingID)
}

const programMatchColumns = `p.site, p.program_id, p.network_id, p.service_id, p.start_at, p.duration_ms, p.name, p.is_free`

func scanProgramMatch(rows *sql.Rows) (store.ProgramMatch, error) {
	var p store.ProgramMatch
	err := rows.Scan(&p.Site, &p.ProgramID, &p.NetworkID, &p.ServiceID, &p.StartAt, &p.DurationMs, &p.Name, &p.IsFree)
	return p, err
}

func queryEPGRows[T any](ctx context.Context, db *sql.DB, entity, query string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", entity, err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]T, 0)
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", entity, err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", entity, err)
	}
	return results, nil
}

var _ store.EPGSync = (*EPG)(nil)
var _ store.EPGReader = (*EPG)(nil)
var _ store.ProgramSearch = (*EPG)(nil)
