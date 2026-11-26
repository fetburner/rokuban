package mysqlstore

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

// encodableProfileSQL は Postgres の encodable_profile に当たる述語の SQL 断片と引数を返す。
// 録画 recCol の profile profCol が known に含まれ、かつ cut に含まれないか、チャプターが確認済み（ownership の行がある）であること。
// ListMissingEncodeProfiles と ListEncodeQueue が、この断片を引いて同じ判定を共有する。
func encodableProfileSQL(recCol, profCol string, known, cut []string) (string, []any) {
	knownIn, knownArgs := inSQL(profCol, known)
	cutNot, cutNotArgs := notInSQL(profCol, cut)
	sql := "(" + knownIn + ") AND (" + cutNot + " OR EXISTS (SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = " + recCol + "))"
	return sql, append(knownArgs, cutNotArgs...)
}

// encodeAttemptsExhaustedSQL は Postgres の encode_attempts_exhausted に当たる述語の SQL 断片を返す。
// (recCol, profCol) の failed 試行が上限に達したかを表す。引数は上限の 1 つで、呼び出し側が末尾に積む。
// 否定して使うときは呼び出し側が NOT を付ける。
func encodeAttemptsExhaustedSQL(recCol, profCol string) string {
	return "EXISTS (SELECT 1 FROM recording_encode_attempts a WHERE a.recording_id = " + recCol +
		" AND a.profile = " + profCol + " AND a.state = 'failed' AND a.attempt_count >= ?)"
}

// ListMissingEncodeProfiles は不足と満たされていない作り直しを合わせた候補をページ単位で返す。
// 録画は pending の候補のうち recording_id の小さい順に row_limit 件まで。
// 引数の並びは、CTE の中の ? の出現順に合わせる（rebuilds → missing → 最後の LIMIT）。
func (s *Media) ListMissingEncodeProfiles(ctx context.Context, params store.MissingEncodeProfilesParams) ([]store.MissingEncodeProfile, error) {
	known := nonNilProfiles(params.KnownProfiles)
	cut := nonNilProfiles(params.CutProfiles)
	rec := nullableRecordingID(params.RecordingID)
	cutIn, cutArgs := inSQL("q.profile", cut)
	encodable, encodableArgs := encodableProfileSQL("u.recording_id", "u.profile", known, cut)
	rebuildExhausted := encodeAttemptsExhaustedSQL("q.recording_id", "q.profile")
	missingExhausted := encodeAttemptsExhaustedSQL("u.recording_id", "u.profile")

	var args []any
	args = append(args, cutArgs...)
	args = append(args, params.AfterRecordingID, rec, rec, params.AttemptLimit)
	args = append(args, params.AfterRecordingID, rec, rec)
	args = append(args, encodableArgs...)
	args = append(args, params.AttemptLimit)
	args = append(args, params.RowLimit)

	rows, err := s.db.QueryContext(ctx, `
WITH rebuilds AS (
    SELECT q.recording_id, q.profile, q.requested_keep, TRUE AS is_rebuild, CAST(NULL AS SIGNED) AS ord
    FROM unsatisfied_encode_rebuilds q
    JOIN encodable_recordings er ON er.recording_id = q.recording_id
    WHERE `+cutIn+`
      AND q.recording_id > ?
      AND (? IS NULL OR q.recording_id = ?)
      AND NOT `+rebuildExhausted+`
), missing AS (
    SELECT u.recording_id, u.profile, CAST(NULL AS JSON) AS requested_keep, FALSE AS is_rebuild, MIN(u.ordinality) AS ord
    FROM unencoded_desired_profiles u
    JOIN encodable_recordings er ON er.recording_id = u.recording_id
    WHERE u.recording_id > ?
      AND (? IS NULL OR u.recording_id = ?)
      AND `+encodable+`
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds rb
          WHERE rb.recording_id = u.recording_id AND rb.profile = u.profile
      )
      AND NOT `+missingExhausted+`
    GROUP BY u.recording_id, u.profile
), pending AS (
    SELECT * FROM rebuilds
    UNION ALL
    SELECT * FROM missing
)
SELECT p.recording_id, p.profile, p.requested_keep, p.is_rebuild
FROM pending p
JOIN (
    SELECT DISTINCT recording_id FROM pending ORDER BY recording_id LIMIT ?
) cand ON cand.recording_id = p.recording_id
ORDER BY p.recording_id, p.is_rebuild DESC,
         CASE WHEN p.is_rebuild THEN p.profile END,
         p.ord`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing missing encode profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.MissingEncodeProfile{}
	for rows.Next() {
		var m store.MissingEncodeProfile
		var raw []byte
		if err := rows.Scan(&m.RecordingID, &m.Profile, &raw, &m.IsRebuild); err != nil {
			return nil, fmt.Errorf("scanning missing encode profiles: %w", err)
		}
		if m.IsRebuild {
			m.RequestedKeep, m.DecodeErr = decodeKeepRanges(raw)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading missing encode profiles: %w", err)
	}
	return out, nil
}
