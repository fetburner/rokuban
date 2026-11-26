package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.MediaReader    = (*Media)(nil)
	_ store.IngestPrecheck = (*Media)(nil)
)

// HasOriginalMediaAsset は kind='original' の media_asset 行があれば true を返す。state は問わない。
func (s *Media) HasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	var found bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM media_assets WHERE recording_id = ? AND kind = 'original')`, recordingID).Scan(&found); err != nil {
		return false, fmt.Errorf("checking original media asset for recording %d: %w", recordingID, err)
	}
	return found, nil
}

// GetIngestOriginalRelPath は置き場にある原本コピーの rel_path を state に関係なく返す。
// 同じ置き場に複数あるときは active を、次に新しい行を選ぶ（pgstore と同じ順）。無ければ store.ErrNotFound。
func (s *Media) GetIngestOriginalRelPath(ctx context.Context, recordingID int64) (string, error) {
	var relPath string
	err := s.db.QueryRowContext(ctx, `
SELECT c.rel_path
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE a.recording_id = ? AND a.kind = 'original' AND c.store = ?
ORDER BY (c.state = 'active') DESC, c.created_at DESC, c.id DESC
LIMIT 1`, recordingID, s.storeName).Scan(&relPath)
	if err != nil {
		return "", mediaQueryError(err, fmt.Sprintf("getting ingest original rel_path for recording %d in store %q", recordingID, s.storeName))
	}
	return relPath, nil
}

// GetLiveMediaAssetRecordingID は state <> 'deleted' の rel_path 所有者を返す。置き場が空なら置き場で絞らない。
// 検査は contracttest.RunMedia の「live rel_path precheck skips tombstones and counts deleting rows」。
func (s *Media) GetLiveMediaAssetRecordingID(ctx context.Context, relPath string) (int64, error) {
	var recordingID int64
	err := s.db.QueryRowContext(ctx, `
SELECT a.recording_id
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.rel_path_key = UNHEX(SHA2(?, 256)) AND c.state <> 'deleted'
  AND (? IS NULL OR c.store = ?)
LIMIT 1`, relPath, nullableString(s.storeName), nullableString(s.storeName)).Scan(&recordingID)
	if err != nil {
		return 0, mediaQueryError(err, fmt.Sprintf("loading live media asset for rel_path %q", relPath))
	}
	return recordingID, nil
}

// ListEncodeQueue は現在の設定で未生成の encoded と実行状態を返す。pgstore の ListEncodeQueue と同じ述語を使う。
// 作り直しの側には cut の確認条件を掛けない（押した時点の keep が確認そのもの）。
func (s *Media) ListEncodeQueue(ctx context.Context, params store.EncodeQueueParams) ([]store.EncodeQueueEntry, error) {
	known := nonNilProfiles(params.KnownProfiles)
	cut := nonNilProfiles(params.CutProfiles)
	knownIn, knownArgs := inSQL("u.profile", known)
	cutNot, cutNotArgs := notInSQL("u.profile", cut)
	cutIn, cutInArgs := inSQL("q.profile", cut)

	var args []any
	args = append(args, knownArgs...)
	args = append(args, cutNotArgs...)
	args = append(args, cutInArgs...)
	args = append(args, params.AttemptLimit)

	rows, err := s.db.QueryContext(ctx, `
WITH pending AS (
    SELECT u.recording_id, u.profile
    FROM unencoded_desired_profiles u
    JOIN encodable_recordings er ON er.recording_id = u.recording_id
    WHERE `+knownIn+`
      AND (`+cutNot+` OR EXISTS (
          SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = u.recording_id))
    UNION
    SELECT q.recording_id, q.profile
    FROM unsatisfied_encode_rebuilds q
    WHERE `+cutIn+`
)
SELECT m.recording_id, m.profile,
       EXISTS (
           SELECT 1 FROM recording_encode_attempts a
           WHERE a.recording_id = m.recording_id AND a.profile = m.profile AND a.state = 'running'
       ) AS running
FROM pending m
JOIN encodable_recordings er ON er.recording_id = m.recording_id
WHERE NOT EXISTS (
    SELECT 1 FROM recording_encode_attempts a
    WHERE a.recording_id = m.recording_id AND a.profile = m.profile
      AND a.state = 'failed' AND a.attempt_count >= ?
)
ORDER BY m.recording_id, m.profile`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing encode queue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.EncodeQueueEntry{}
	for rows.Next() {
		var e store.EncodeQueueEntry
		var profile string
		if err := rows.Scan(&e.RecordingID, &profile, &e.Running); err != nil {
			return nil, fmt.Errorf("scanning encode queue: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading encode queue: %w", err)
	}
	return out, nil
}

// ListAwaitingReviewRecordingIDs は確認待ちの録画 ID を重複なしで、小さい順に返す。
func (s *Media) ListAwaitingReviewRecordingIDs(ctx context.Context, cutProfiles []string) ([]int64, error) {
	cutIn, args := inSQL("a.profile", nonNilProfiles(cutProfiles))
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT a.recording_id
FROM awaiting_review_candidates a
WHERE `+cutIn+`
ORDER BY a.recording_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing recordings awaiting chapter review: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning recordings awaiting chapter review: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recordings awaiting chapter review: %w", err)
	}
	return out, nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。計測済みの判定は current_ts_scanned_originals に寄せる。
func (s *Media) ListRecordingDropStats(ctx context.Context, recordingID int64) ([]store.RecordingDropStat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.pid, d.packets, d.drops, d.errors, d.scrambled, d.pid_type
FROM drop_stats d
JOIN current_ts_scanned_originals s ON s.media_asset_id = d.media_asset_id
JOIN media_assets a ON a.id = d.media_asset_id
WHERE a.recording_id = ?
ORDER BY d.pid`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing drop stats for recording %d: %w", recordingID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.RecordingDropStat{}
	for rows.Next() {
		var stat store.RecordingDropStat
		var pidType sql.NullString
		if err := rows.Scan(&stat.Pid, &stat.Packets, &stat.Drops, &stat.Errors, &stat.Scrambled, &pidType); err != nil {
			return nil, fmt.Errorf("scanning drop stats for recording %d: %w", recordingID, err)
		}
		if pidType.Valid {
			stat.PidType = &pidType.String
		}
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading drop stats for recording %d: %w", recordingID, err)
	}
	return out, nil
}

// ListRecordingDropPositions は録画のドロップ観測位置を PID 別に返す。
func (s *Media) ListRecordingDropPositions(ctx context.Context, recordingID int64) ([]store.RecordingDropPosition, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT p.pid, p.byte_offset, p.elapsed_ms
FROM drop_positions p
JOIN media_assets a ON a.id = p.media_asset_id
WHERE a.recording_id = ? AND a.kind = 'original'
ORDER BY p.pid, p.byte_offset`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing drop positions for recording %d: %w", recordingID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.RecordingDropPosition{}
	for rows.Next() {
		var pos store.RecordingDropPosition
		var elapsed sql.NullInt64
		if err := rows.Scan(&pos.Pid, &pos.ByteOffset, &elapsed); err != nil {
			return nil, fmt.Errorf("scanning drop positions for recording %d: %w", recordingID, err)
		}
		if elapsed.Valid {
			pos.ElapsedMs = &elapsed.Int64
		}
		out = append(out, pos)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading drop positions for recording %d: %w", recordingID, err)
	}
	return out, nil
}
