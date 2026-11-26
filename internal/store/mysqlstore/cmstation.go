package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// Analysis は CM 検出の局単位の状態（ロゴの枠・候補・学習済みロゴ）と、候補解析の入力を MySQL で読み書きする。
// pgstore の Analysis と同じ役割で、CMDetection の残りのメソッドも後のタスクで同じ型に足す。
type Analysis struct {
	db        *sql.DB
	storeName string
}

var _ store.CMStation = (*Analysis)(nil)

// NewAnalysis は storeName の置き場を読む Analysis を返す。storeName が空なら全置き場を見る。
func NewAnalysis(db *sql.DB, storeName string) *Analysis {
	return &Analysis{db: db, storeName: storeName}
}

// storeArg は置き場の絞り込みに渡す値を返す。置き場が空なら NULL にし、SQL の (? IS NULL OR ...) が全置き場とみなす。
// 置き場を使う SQL は、この値を 2 回渡す。
func (s *Analysis) storeArg() any {
	if s.storeName == "" {
		return nil
	}
	return s.storeName
}

// recordingDurationSQL は録画 r の尺（ミリ秒）。実尺があれば実尺、無ければ番組尺で、負にはしない。
// pgstore の GetCMRecordingDuration と同じ式で、実尺はミリ秒未満を四捨五入する（Postgres の ::bigint と同じ）。
const recordingDurationSQL = `GREATEST(CAST(CASE WHEN r.started_at IS NOT NULL AND r.ended_at IS NOT NULL
    THEN TIMESTAMPDIFF(MICROSECOND, r.started_at, r.ended_at) / 1000
    ELSE r.program_duration_ms END AS SIGNED), 0)`

// cmStationLockName は局のロック用の行の名前。
func cmStationLockName(station store.CMStationRef) string {
	return fmt.Sprintf("cm_station:%d:%d", station.NetworkID, station.ServiceID)
}

// lockCMStation は局のロック用の行を排他で取る（pgstore の pg_advisory_xact_lock に当たる）。
// 行が無ければ先に作る。行が無い局への並行する 2 本の INSERT は、片方が commit するまで待ち、
// 待った側は重複として行を更新に回る（TestCMStationLockRowCreatedConcurrently が検査する）。
// 局の状態（枠・ロゴ・候補）を読む前にこれを呼ぶ。録画の行だけを先に読むのは構わない（SaveLogoArea がそうしている）。
func lockCMStation(ctx context.Context, tx *sql.Tx, station store.CMStationRef) error {
	name := cmStationLockName(station)
	if _, err := tx.ExecContext(ctx, `INSERT INTO named_locks (name) VALUES (?) AS incoming ON DUPLICATE KEY UPDATE name = incoming.name`, name); err != nil {
		return fmt.Errorf("creating station lock for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM named_locks WHERE name = ? FOR UPDATE`, name).Scan(&locked); err != nil {
		return fmt.Errorf("locking station for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return nil
}

// queryRower は *sql.DB と *sql.Tx の両方で使う読み取りの口。
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// loadLogoArea は局の枠を読む。無ければ sql.ErrNoRows をそのまま返し、呼び出し側が意味を決める。
func loadLogoArea(ctx context.Context, q queryRower, station store.CMStationRef) (store.CMLogoArea, error) {
	var area store.CMLogoArea
	err := q.QueryRowContext(ctx, `SELECT x, y, w, h, coded_width, coded_height, updated_at
FROM cm_logo_areas WHERE network_id = ? AND service_id = ?`, station.NetworkID, station.ServiceID).
		Scan(&area.X, &area.Y, &area.W, &area.H, &area.CodedWidth, &area.CodedHeight, &area.UpdatedAt)
	return area, err
}

// loadLogoCandidate は局の候補を読む。無ければ sql.ErrNoRows をそのまま返す。
func loadLogoCandidate(ctx context.Context, q queryRower, station store.CMStationRef) (store.CMLogoCandidate, error) {
	var c store.CMLogoCandidate
	err := q.QueryRowContext(ctx, `SELECT state, stage, error, x, y, w, h, coded_width, coded_height,
    recording_id, observed_area_updated_at, lgd, preview_png, attempted_at
FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`, station.NetworkID, station.ServiceID).
		Scan(&c.State, &c.Stage, &c.Error, &c.X, &c.Y, &c.W, &c.H, &c.CodedWidth, &c.CodedHeight,
			&c.RecordingID, &c.ObservedAreaUpdatedAt, &c.LGD, &c.PreviewPNG, &c.AttemptedAt)
	return c, err
}

// recordingDuration は録画の尺をミリ秒で返す。録画が無ければ sql.ErrNoRows を返す。
func recordingDuration(ctx context.Context, q queryRower, recordingID int64) (int64, error) {
	var ms int64
	err := q.QueryRowContext(ctx, `SELECT `+recordingDurationSQL+` FROM recordings r WHERE r.id = ?`, recordingID).Scan(&ms)
	return ms, err
}

// hasPresentOriginal は録画が、読み出す置き場に実体のある原本を持つかを返す。
// 録画の削除・消去の判定は呼び出し側が行う。
func (s *Analysis) hasPresentOriginal(ctx context.Context, q queryRower, recordingID int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM recordings r
    JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
    WHERE r.id = ? AND r.deleted_at IS NULL AND r.purged_at IS NULL
      AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c
                  WHERE c.media_asset_id = o.id AND (? IS NULL OR c.store = ?))
)`, recordingID, s.storeArg(), s.storeArg()).Scan(&ok)
	return ok, err
}

// cmWorkItemRow は録画と原本の検出入力を 1 行で読んだもの。GetWorkItem と GetCandidateRecording が共有する。
type cmWorkItemRow struct {
	ID              int64
	NetworkID       int32
	ServiceID       int32
	RelPath         *string
	CMDetect        bool
	IsTrashed       bool
	OriginalMissing bool
	Detected        bool
}

// cmWorkItem は録画の検出入力を読む。読む置き場の原本コピーは置き場名順の先頭の active コピー
// （pgstore の preferred_active_media_asset_copy と同じ選び方）。録画が無ければ store.ErrNotFound。
func (s *Analysis) cmWorkItem(ctx context.Context, recordingID int64) (cmWorkItemRow, error) {
	var row cmWorkItemRow
	err := s.db.QueryRowContext(ctx, `
SELECT r.id, r.network_id, r.service_id, copy.rel_path,
       COALESCE(p.cm_detect, FALSE),
       r.deleted_at IS NOT NULL,
       (copy.id IS NULL OR EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id)),
       EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
FROM recordings r
LEFT JOIN recording_processing_policy p ON p.recording_id = r.id
LEFT JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
LEFT JOIN media_asset_copies copy
  ON copy.id = (SELECT c.id FROM active_media_asset_copies c
                WHERE c.media_asset_id = o.id AND (? IS NULL OR c.store = ?)
                ORDER BY c.store LIMIT 1)
WHERE r.id = ?`, s.storeArg(), s.storeArg(), recordingID).
		Scan(&row.ID, &row.NetworkID, &row.ServiceID, &row.RelPath, &row.CMDetect,
			&row.IsTrashed, &row.OriginalMissing, &row.Detected)
	if errors.Is(err, sql.ErrNoRows) {
		return cmWorkItemRow{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return cmWorkItemRow{}, fmt.Errorf("loading CM detection work item for recording %d: %w", recordingID, err)
	}
	return row, nil
}

// GetWorkItem は録画と有効な原本の検出入力を返す。録画が無ければ store.ErrNotFound。
func (s *Analysis) GetWorkItem(ctx context.Context, recordingID int64) (store.CMDetectionWorkItem, error) {
	row, err := s.cmWorkItem(ctx, recordingID)
	if err != nil {
		return store.CMDetectionWorkItem{}, err
	}
	return store.CMDetectionWorkItem{
		ID:              row.ID,
		Station:         store.CMStationRef{NetworkID: row.NetworkID, ServiceID: row.ServiceID},
		RelPath:         row.RelPath,
		Enabled:         row.CMDetect,
		IsTrashed:       row.IsTrashed,
		OriginalMissing: row.OriginalMissing,
		Detected:        row.Detected,
	}, nil
}

// GetCandidateRecording は候補解析に必要な録画と原本の状態を返す。録画が無ければ store.ErrNotFound。
func (s *Analysis) GetCandidateRecording(ctx context.Context, recordingID int64) (store.CMLogoCandidateRecording, error) {
	row, err := s.cmWorkItem(ctx, recordingID)
	if err != nil {
		return store.CMLogoCandidateRecording{}, err
	}
	return store.CMLogoCandidateRecording{
		ID: row.ID, NetworkID: row.NetworkID, ServiceID: row.ServiceID,
		RelPath: row.RelPath, IsTrashed: row.IsTrashed, OriginalMissing: row.OriginalMissing,
	}, nil
}

// ListMissingLogoCandidates は desired な候補解析を (network_id, service_id) 順に先頭から limit 件返す。
func (s *Analysis) ListMissingLogoCandidates(ctx context.Context, limit int32) ([]store.CMLogoCandidateJob, error) {
	return queryRows(ctx, s.db, "CM logo candidate jobs", `
SELECT desired.network_id, desired.service_id, desired.recording_id, desired.area_updated_at, `+recordingDurationSQL+`
FROM cm_logo_candidate_desired desired
JOIN recordings r ON r.id = desired.recording_id
ORDER BY desired.network_id, desired.service_id
LIMIT ?`,
		func(rows *sql.Rows) (store.CMLogoCandidateJob, error) {
			var job store.CMLogoCandidateJob
			err := rows.Scan(&job.Station.NetworkID, &job.Station.ServiceID, &job.RecordingID, &job.AreaUpdatedAt, &job.RecordingDurationMs)
			return job, err
		}, limit)
}

// ListCMLogoStates はロゴ、枠、候補および CM 検出の局別状態を返す。
// 局の表示名と site は最新（id が最大）の未削除録画から取る。
// 実体の有る原本を持つ録画だけを、フレームの元と再検出の対象として数える。
func (s *Analysis) ListCMLogoStates(ctx context.Context) ([]store.CMLogoState, error) {
	return queryRows(ctx, s.db, "CM logo states", `
WITH live AS (
    SELECT r.id, r.network_id, r.service_id, r.service_name, r.site
    FROM recordings r
    WHERE r.deleted_at IS NULL
),
present_originals AS (
    SELECT DISTINCT o.recording_id
    FROM media_assets o
    JOIN active_present_media_asset_copies c ON c.media_asset_id = o.id
    WHERE o.kind = 'original' AND (? IS NULL OR c.store = ?)
),
stations AS (
    SELECT network_id, service_id, COUNT(*) AS recording_count, MAX(id) AS latest_id
    FROM live
    GROUP BY network_id, service_id
),
failed_attempts AS (
    SELECT l.network_id, l.service_id, ca.recording_id, ca.stage, ca.attempted_at
    FROM live l
    JOIN current_cm_attempts ca ON ca.recording_id = l.id
    WHERE ca.state = 'failed' AND NOT (ca.stage <=> 'adopt')
      AND NOT EXISTS (SELECT 1 FROM cm_detection_desired d WHERE d.recording_id = l.id)
),
failed AS (
    SELECT network_id, service_id, COUNT(DISTINCT recording_id) AS failed_count
    FROM failed_attempts
    GROUP BY network_id, service_id
),
last_failure AS (
    SELECT network_id, service_id, stage
    FROM (
        SELECT network_id, service_id, stage,
               ROW_NUMBER() OVER (PARTITION BY network_id, service_id ORDER BY attempted_at DESC) AS rn
        FROM failed_attempts
    ) ranked
    WHERE rn = 1
),
pending AS (
    SELECT l.network_id, l.service_id, COUNT(DISTINCT d.recording_id) AS pending_count
    FROM live l
    JOIN cm_detection_desired d ON d.recording_id = l.id
    GROUP BY l.network_id, l.service_id
),
detected AS (
    SELECT l.network_id, l.service_id,
           COUNT(DISTINCT d.recording_id) AS detected_count,
           COUNT(DISTINCT p.recording_id) AS redetectable_count
    FROM live l
    JOIN recording_cm_detections d ON d.recording_id = l.id
    LEFT JOIN present_originals p ON p.recording_id = l.id
    GROUP BY l.network_id, l.service_id
),
frames AS (
    SELECT l.network_id, l.service_id, MAX(l.id) AS frame_recording_id
    FROM live l
    JOIN present_originals p ON p.recording_id = l.id
    GROUP BY l.network_id, l.service_id
)
SELECT s.network_id, s.service_id,
       latest.service_name, latest.site,
       s.recording_count,
       logo.learned_at, logo.preview_png,
       COALESCE(f.failed_count, 0),
       COALESCE(pe.pending_count, 0),
       COALESCE(lf.stage, ''),
       COALESCE(dt.detected_count, 0),
       COALESCE(dt.redetectable_count, 0),
       logo.coded_width, logo.coded_height,
       area.x, area.y, area.w, area.h, area.coded_width, area.coded_height, area.updated_at,
       cand.state, cand.stage, cand.error,
       cand.x, cand.y, cand.w, cand.h, cand.coded_width, cand.coded_height,
       cand.preview_png, cand.recording_id, cand.attempted_at,
       COALESCE(fr.frame_recording_id, 0)
FROM stations s
JOIN live latest ON latest.id = s.latest_id
LEFT JOIN cm_logos logo ON logo.network_id = s.network_id AND logo.service_id = s.service_id
LEFT JOIN cm_logo_areas area ON area.network_id = s.network_id AND area.service_id = s.service_id
LEFT JOIN cm_logo_candidates cand ON cand.network_id = s.network_id AND cand.service_id = s.service_id
LEFT JOIN failed f ON f.network_id = s.network_id AND f.service_id = s.service_id
LEFT JOIN pending pe ON pe.network_id = s.network_id AND pe.service_id = s.service_id
LEFT JOIN last_failure lf ON lf.network_id = s.network_id AND lf.service_id = s.service_id
LEFT JOIN detected dt ON dt.network_id = s.network_id AND dt.service_id = s.service_id
LEFT JOIN frames fr ON fr.network_id = s.network_id AND fr.service_id = s.service_id
ORDER BY s.network_id, s.service_id`,
		func(rows *sql.Rows) (store.CMLogoState, error) {
			var st store.CMLogoState
			var areaX, areaY, areaW, areaH, areaCodedW, areaCodedH *int32
			var areaUpdatedAt *time.Time
			var candState, candStage, candError *string
			var candX, candY, candW, candH, candCodedW, candCodedH *int32
			var candPreview []byte
			var candRecordingID *int64
			var candAttemptedAt *time.Time
			err := rows.Scan(
				&st.NetworkID, &st.ServiceID,
				&st.ServiceName, &st.Site,
				&st.RecordingCount,
				&st.LearnedAt, &st.PreviewPNG,
				&st.FailedCount,
				&st.PendingCount,
				&st.LastFailureStage,
				&st.DetectedCount,
				&st.RedetectableCount,
				&st.LearnedCodedWidth, &st.LearnedCodedHeight,
				&areaX, &areaY, &areaW, &areaH, &areaCodedW, &areaCodedH, &areaUpdatedAt,
				&candState, &candStage, &candError,
				&candX, &candY, &candW, &candH, &candCodedW, &candCodedH,
				&candPreview, &candRecordingID, &candAttemptedAt,
				&st.FrameRecordingID,
			)
			if err != nil {
				return st, err
			}
			if areaX != nil {
				st.Area = &store.CMLogoArea{
					X: *areaX, Y: *areaY, W: *areaW, H: *areaH,
					CodedWidth: *areaCodedW, CodedHeight: *areaCodedH, UpdatedAt: *areaUpdatedAt,
				}
			}
			if candState != nil {
				st.Candidate = &store.CMLogoCandidateSummary{
					State: *candState, Stage: candStage, Error: candError,
					X: *candX, Y: *candY, W: *candW, H: *candH,
					CodedWidth: *candCodedW, CodedHeight: *candCodedH,
					RecordingID: candRecordingID, PreviewPNG: candPreview, AttemptedAt: *candAttemptedAt,
				}
			}
			return st, nil
		}, s.storeArg(), s.storeArg())
}

// GetLogoArea は局のロゴ枠を返す。無ければ store.ErrNotFound。
func (s *Analysis) GetLogoArea(ctx context.Context, station store.CMStationRef) (store.CMLogoArea, error) {
	area, err := loadLogoArea(ctx, s.db, station)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CMLogoArea{}, fmt.Errorf("CM logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
	}
	if err != nil {
		return store.CMLogoArea{}, fmt.Errorf("loading CM logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return area, nil
}

// GetLogo は局の学習済みロゴを返す。無ければ store.ErrNotFound。
func (s *Analysis) GetLogo(ctx context.Context, station store.CMStationRef) (store.CMLogo, error) {
	var logo store.CMLogo
	err := s.db.QueryRowContext(ctx, `SELECT lgd, coded_width, coded_height, learned_at
FROM cm_logos WHERE network_id = ? AND service_id = ?`, station.NetworkID, station.ServiceID).
		Scan(&logo.LGD, &logo.CodedWidth, &logo.CodedHeight, &logo.LearnedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CMLogo{}, fmt.Errorf("CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
	}
	if err != nil {
		return store.CMLogo{}, fmt.Errorf("loading CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return logo, nil
}

// GetLogoCandidate は局の現在の候補を返す。無ければ store.ErrNotFound。
func (s *Analysis) GetLogoCandidate(ctx context.Context, station store.CMStationRef) (store.CMLogoCandidate, error) {
	c, err := loadLogoCandidate(ctx, s.db, station)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CMLogoCandidate{}, fmt.Errorf("CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
	}
	if err != nil {
		return store.CMLogoCandidate{}, fmt.Errorf("loading CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return c, nil
}

// SaveLogoArea は局ロック下で枠を保存し、既存候補を消して解析ヒントを返す。
// 解析元の録画は同じ局に属し、実体の有る原本を持つ必要がある。
func (s *Analysis) SaveLogoArea(ctx context.Context, station store.CMStationRef, input store.CMLogoAreaInput) (store.CMLogoCandidateJob, error) {
	var job store.CMLogoCandidateJob
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var networkID, serviceID int32
		err := tx.QueryRowContext(ctx, `SELECT network_id, service_id FROM recordings WHERE id = ?`, input.RecordingID).
			Scan(&networkID, &serviceID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", input.RecordingID,
				errors.Join(store.ErrNotFound, store.ErrCMLogoRecordingNotFound))
		}
		if err != nil {
			return fmt.Errorf("loading analysis recording %d: %w", input.RecordingID, err)
		}
		if networkID != station.NetworkID || serviceID != station.ServiceID {
			return fmt.Errorf("recording %d: %w", input.RecordingID, store.ErrCMLogoRecordingWrongStation)
		}
		active, err := s.hasPresentOriginal(ctx, tx, input.RecordingID)
		if err != nil {
			return fmt.Errorf("checking analysis recording %d: %w", input.RecordingID, err)
		}
		if !active {
			return fmt.Errorf("recording %d: %w", input.RecordingID, store.ErrCMLogoRecordingOriginalUnavailable)
		}
		if err := lockCMStation(ctx, tx, station); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS incoming
ON DUPLICATE KEY UPDATE x = incoming.x, y = incoming.y, w = incoming.w, h = incoming.h,
    coded_width = incoming.coded_width, coded_height = incoming.coded_height,
    updated_at = CURRENT_TIMESTAMP(6)`,
			station.NetworkID, station.ServiceID, input.X, input.Y, input.W, input.H,
			input.CodedWidth, input.CodedHeight); err != nil {
			return fmt.Errorf("saving logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		area, err := loadLogoArea(ctx, tx, station)
		if err != nil {
			return fmt.Errorf("loading saved logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		durationMs, err := recordingDuration(ctx, tx, input.RecordingID)
		if err != nil {
			return fmt.Errorf("getting recording %d duration for CM logo candidate: %w", input.RecordingID, err)
		}
		job = store.CMLogoCandidateJob{
			Station: station, RecordingID: input.RecordingID,
			RecordingDurationMs: durationMs, AreaUpdatedAt: area.UpdatedAt,
		}
		return nil
	})
	if err != nil {
		return store.CMLogoCandidateJob{}, err
	}
	return job, nil
}

// DeleteLogoArea は局の採用待ち試行と枠を局ロック下で原子的に消す。候補は枠の外部キーの CASCADE で消える。
func (s *Analysis) DeleteLogoArea(ctx context.Context, station store.CMStationRef) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, station); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE ca FROM recording_cm_attempts ca
JOIN recordings r ON r.id = ca.recording_id
WHERE r.network_id = ? AND r.service_id = ? AND ca.stage = 'adopt'`, station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("clearing adoption waits for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_areas WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("deleting logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		return nil
	})
}

// DeleteLogo は局ロック下でロゴを消し、次の候補解析先があれば返す。
// 枠が残っていて実体の有る原本の録画があれば、その録画への候補解析を返す。
func (s *Analysis) DeleteLogo(ctx context.Context, station store.CMStationRef) (store.CMLogoDeleteResult, error) {
	var result store.CMLogoDeleteResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, station); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logos WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("deleting CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		area, areaErr := loadLogoArea(ctx, tx, station)
		if errors.Is(areaErr, sql.ErrNoRows) {
			return nil
		}
		if areaErr != nil {
			return fmt.Errorf("loading logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, areaErr)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		var recordingID int64
		err := tx.QueryRowContext(ctx, `
SELECT r.id
FROM recordings r
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
WHERE r.network_id = ? AND r.service_id = ?
  AND r.deleted_at IS NULL AND r.purged_at IS NULL
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c
              WHERE c.media_asset_id = o.id AND (? IS NULL OR c.store = ?))
ORDER BY r.id DESC
LIMIT 1`, station.NetworkID, station.ServiceID, s.storeArg(), s.storeArg()).Scan(&recordingID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("finding analysis recording for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		durationMs, err := recordingDuration(ctx, tx, recordingID)
		if err != nil {
			return fmt.Errorf("getting recording %d duration for CM logo candidate: %w", recordingID, err)
		}
		result.CandidateJob = &store.CMLogoCandidateJob{
			Station: station, RecordingID: recordingID,
			RecordingDurationMs: durationMs, AreaUpdatedAt: area.UpdatedAt,
		}
		return nil
	})
	if err != nil {
		return store.CMLogoDeleteResult{}, err
	}
	return result, nil
}

// DeleteLogoCandidate は局ロック下で現在の候補を消す。
func (s *Analysis) DeleteLogoCandidate(ctx context.Context, station store.CMStationRef) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, station); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("deleting CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
		}
		return nil
	})
}

// AdoptLogoCandidate は局ロック下で候補を検証・採用し、必要なら検出結果も消す。
// 候補が ready で、学習に使った枠の版が現在の枠と同じときだけ採用する。
func (s *Analysis) AdoptLogoCandidate(ctx context.Context, station store.CMStationRef, redetect bool) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, station); err != nil {
			return err
		}
		candidate, err := loadLogoCandidate(ctx, tx, station)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateUnavailable}
		}
		if err != nil {
			return fmt.Errorf("loading CM logo candidate: %w", err)
		}
		if candidate.State != "ready" {
			return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateNotReady, State: candidate.State}
		}
		area, err := loadLogoArea(ctx, tx, station)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionAreaMissing}
		}
		if err != nil {
			return fmt.Errorf("loading taught logo area: %w", err)
		}
		if !candidate.ObservedAreaUpdatedAt.Equal(area.UpdatedAt) {
			return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionAreaChanged}
		}
		if candidate.RecordingID == nil || len(candidate.LGD) == 0 {
			return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateUnusable}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cm_logos (network_id, service_id, lgd, preview_png, learned_from, coded_width, coded_height)
VALUES (?, ?, ?, ?, ?, ?, ?) AS incoming
ON DUPLICATE KEY UPDATE lgd = incoming.lgd, preview_png = incoming.preview_png,
    learned_at = CURRENT_TIMESTAMP(6), learned_from = incoming.learned_from,
    coded_width = incoming.coded_width, coded_height = incoming.coded_height`,
			station.NetworkID, station.ServiceID, candidate.LGD, candidate.PreviewPNG,
			*candidate.RecordingID, candidate.CodedWidth, candidate.CodedHeight); err != nil {
			return fmt.Errorf("adopting CM logo candidate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID); err != nil {
			return fmt.Errorf("deleting adopted CM logo candidate: %w", err)
		}
		if redetect {
			if _, err := tx.ExecContext(ctx, `
DELETE d FROM recording_cm_detections d
JOIN recordings r ON r.id = d.recording_id
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
WHERE r.network_id = ? AND r.service_id = ?
  AND r.deleted_at IS NULL AND r.purged_at IS NULL
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c
              WHERE c.media_asset_id = o.id AND (? IS NULL OR c.store = ?))`,
				station.NetworkID, station.ServiceID, s.storeArg(), s.storeArg()); err != nil {
				return fmt.Errorf("clearing active CM detections after adoption: %w", err)
			}
		}
		return nil
	})
}

// BeginLogoCandidate は局ロック下で desired を再確認し running 試行を作る。
// desired でなければ started は false で、何も書かない。
func (s *Analysis) BeginLogoCandidate(ctx context.Context, station store.CMStationRef, recordingID int64, areaUpdatedAt time.Time) (time.Time, bool, error) {
	var attemptedAt time.Time
	var started bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, station); err != nil {
			return fmt.Errorf("CM logo candidate: locking station: %w", err)
		}
		var desired bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM cm_logo_candidate_desired
               WHERE network_id = ? AND service_id = ? AND area_updated_at = ?)`,
			station.NetworkID, station.ServiceID, areaUpdatedAt).Scan(&desired); err != nil {
			return fmt.Errorf("CM logo candidate: checking desired state: %w", err)
		}
		if !desired {
			return nil
		}
		// 挿入の行数で開始を判定する。行が無い（枠が消えた・学習済み）なら何も書かれない。
		res, err := tx.ExecContext(ctx, `
INSERT INTO cm_logo_candidates (network_id, service_id, state, stage, error,
    x, y, w, h, coded_width, coded_height, recording_id, observed_area_updated_at, attempted_at)
SELECT a.network_id, a.service_id, 'running', NULL, NULL,
       a.x, a.y, a.w, a.h, a.coded_width, a.coded_height,
       ?, a.updated_at, CURRENT_TIMESTAMP(6)
FROM cm_logo_areas a
LEFT JOIN cm_logos l ON l.network_id = a.network_id AND l.service_id = a.service_id
WHERE a.network_id = ? AND a.service_id = ?
  AND a.updated_at = ?
  AND NOT EXISTS (SELECT 1 FROM cm_logo_candidates c
                  WHERE c.network_id = a.network_id AND c.service_id = a.service_id)
  AND (l.network_id IS NULL OR l.learned_at < a.updated_at)`,
			recordingID, station.NetworkID, station.ServiceID, areaUpdatedAt)
		if err != nil {
			return fmt.Errorf("CM logo candidate: creating running row: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("CM logo candidate: counting running row: %w", err)
		}
		if n == 0 {
			return nil
		}
		if err := tx.QueryRowContext(ctx, `SELECT attempted_at FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
			station.NetworkID, station.ServiceID).Scan(&attemptedAt); err != nil {
			return fmt.Errorf("CM logo candidate: reading attempt time: %w", err)
		}
		started = true
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return attemptedAt, started, nil
}

// MarkLogoCandidateReady は枠版と試行時刻が一致する候補だけを ready にする。
// 直列化はせず、候補行の試行時刻と枠版の照合で守る。
func (s *Analysis) MarkLogoCandidateReady(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time, lgd, previewPNG []byte) (bool, error) {
	var n int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE cm_logo_candidates c
SET c.state = 'ready', c.stage = NULL, c.error = NULL, c.lgd = ?, c.preview_png = ?
WHERE c.network_id = ? AND c.service_id = ?
  AND c.state = 'running'
  AND c.observed_area_updated_at = ?
  AND c.attempted_at = ?
  AND EXISTS (SELECT 1 FROM cm_logo_areas a
              WHERE a.network_id = c.network_id AND a.service_id = c.service_id
                AND a.updated_at = ?)`,
			lgd, previewPNG, station.NetworkID, station.ServiceID, areaUpdatedAt, attemptedAt, areaUpdatedAt)
		if err != nil {
			return fmt.Errorf("saving CM logo candidate: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkLogoCandidateFailed は候補行の枠版と試行時刻が一致する running 候補だけを failed にする。
// 現在の枠は見ない。
func (s *Analysis) MarkLogoCandidateFailed(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time, stage *string, message string) (bool, error) {
	var n int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE cm_logo_candidates
SET state = 'failed', stage = ?, error = ?
WHERE network_id = ? AND service_id = ?
  AND state = 'running'
  AND observed_area_updated_at = ?
  AND attempted_at = ?`,
			stage, message, station.NetworkID, station.ServiceID, areaUpdatedAt, attemptedAt)
		if err != nil {
			return fmt.Errorf("marking CM logo candidate failed: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DiscardLogoCandidate は候補行の枠版と試行時刻が一致する running 候補を消す。一致しなければ何もしない。
func (s *Analysis) DiscardLogoCandidate(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM cm_logo_candidates
WHERE network_id = ? AND service_id = ?
  AND state = 'running'
  AND observed_area_updated_at = ?
  AND attempted_at = ?`,
			station.NetworkID, station.ServiceID, areaUpdatedAt, attemptedAt); err != nil {
			return fmt.Errorf("discarding CM logo candidate: %w", err)
		}
		return nil
	})
}
