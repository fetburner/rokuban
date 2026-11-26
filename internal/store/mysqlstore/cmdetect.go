package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.CMDetection = (*Analysis)(nil)

// cmAttemptRow は録画の CM 検出試行の行。BeginAttempt が既存の行から次の値を決めるのに使う。
type cmAttemptRow struct {
	State       string
	Count       int32
	Generation  int64
	AttemptedAt time.Time
	Stage       *string
	Error       *string
}

// nextCMAttemptRow は既存の試行行（無ければ nil）から、開始後の行の値を決める。
// pgstore の BeginCMDetectionAttempt の ON CONFLICT 句と同じ判定。MySQL の ON DUPLICATE KEY UPDATE は
// 代入を左から順に評価するので、古い値を参照する判定は Go 側で行う。
func nextCMAttemptRow(prev *cmAttemptRow, generation int64, maxAttempts int32, now time.Time) cmAttemptRow {
	fresh := cmAttemptRow{State: "running", Count: 1, Generation: generation, AttemptedAt: now}
	switch {
	case prev == nil, prev.Generation != generation, prev.State == "failed":
		return fresh
	case prev.State == "running" && prev.Count >= maxAttempts:
		// 上限に達した running は、プロセスが途中で止まった試行とみなして失敗にする。
		stage, msg := "stopped", "CM detection process stopped while job was running"
		return cmAttemptRow{State: "failed", Count: prev.Count, Generation: generation,
			AttemptedAt: prev.AttemptedAt, Stage: &stage, Error: &msg}
	default:
		fresh.Count = prev.Count + 1
		return fresh
	}
}

// isDetectionDesired は録画の現在の CM 検出が desired かを返す。
func isDetectionDesired(ctx context.Context, q queryRower, recordingID int64) (bool, error) {
	var desired bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM cm_detection_desired WHERE recording_id = ?)`, recordingID).Scan(&desired)
	return desired, err
}

// lockRunningAttempt は録画の試行の行を排他で取り、現在の世代で running の一致する試行があるかを返す。
// 行が無い・世代や試行回数が違う・running でないなら false。pgstore の LockCMDetectionAttempt に当たる。
func lockRunningAttempt(ctx context.Context, tx *sql.Tx, attempt store.CMDetectionAttempt) (bool, error) {
	var count int32
	err := tx.QueryRowContext(ctx, `
SELECT attempt_count FROM recording_cm_attempts
WHERE recording_id = ? AND policy_generation = ? AND state = 'running'
FOR UPDATE`, attempt.RecordingID, attempt.PolicyGeneration).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking CM detection attempt fence for recording %d: %w", attempt.RecordingID, err)
	}
	return count == attempt.AttemptCount, nil
}

// GetRecordingDuration は録画の実尺または番組尺をミリ秒で返す。録画が無ければ store.ErrNotFound。
func (s *Analysis) GetRecordingDuration(ctx context.Context, recordingID int64) (int64, error) {
	ms, err := recordingDuration(ctx, s.db, recordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("getting duration for recording %d: %w", recordingID, err)
	}
	return ms, nil
}

// IsDetectionDesired は録画の現在の CM 検出が desired かを返す。
func (s *Analysis) IsDetectionDesired(ctx context.Context, recordingID int64) (bool, error) {
	desired, err := isDetectionDesired(ctx, s.db, recordingID)
	if err != nil {
		return false, fmt.Errorf("checking desired CM detection for recording %d: %w", recordingID, err)
	}
	return desired, nil
}

// BeginAttempt は現在の方針世代で試行を開始し、実行するかを返す。
// 直列化の順序は録画（共有）→ 試行。録画の行は FOR SHARE で取る。SaveDetection は録画を排他で取るので、
// 結果の保存とは録画の行で待ち合う。共有で取るのは、SaveLearnedLogo（局 → 試行 → 録画の外部キー検査の共有）と
// 循環しないためである。並行する開始は録画の行では待たず、試行の行で待ち合う。
// 試行の行は INSERT ... ON DUPLICATE KEY UPDATE の no-op で確保する。並行する初回の開始は、先に入った行の
// commit を待ってから重複として扱われ、ErrConflict にならない。影響行数が 1 なら新しく作った行、
// 0 なら既存の行なので、FOR UPDATE で読み直し、nextCMAttemptRow で決めて更新する。
// 原本の生きたコピーが無い録画は試行を作らず、shouldRun は false・err は nil を返す。
func (s *Analysis) BeginAttempt(ctx context.Context, recordingID int64, maxAttempts int32) (store.CMDetectionAttempt, bool, error) {
	var attempt store.CMDetectionAttempt
	var shouldRun bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRecordingShared(ctx, tx, recordingID); err != nil {
			return err
		}
		item, err := s.cmWorkItem(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		if item.RelPath == nil {
			return nil
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE((SELECT p.cm_detect_generation FROM recording_processing_policy p WHERE p.recording_id = ?), 0)`,
			recordingID).Scan(&generation); err != nil {
			return fmt.Errorf("reading CM detection policy generation for recording %d: %w", recordingID, err)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO recording_cm_attempts (recording_id, state, stage, error, attempted_at, attempt_count, policy_generation)
VALUES (?, 'running', NULL, NULL, ?, 1, ?)
ON DUPLICATE KEY UPDATE recording_id = recording_cm_attempts.recording_id`, recordingID, now, generation)
		if err != nil {
			return fmt.Errorf("creating CM detection attempt row for recording %d: %w", recordingID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting CM detection attempt row for recording %d: %w", recordingID, err)
		}
		var next cmAttemptRow
		if affected == 1 {
			next = nextCMAttemptRow(nil, generation, maxAttempts, now)
		} else {
			var prev cmAttemptRow
			if err := tx.QueryRowContext(ctx, `
SELECT attempt_count, state, attempted_at, policy_generation FROM recording_cm_attempts
WHERE recording_id = ? FOR UPDATE`, recordingID).Scan(&prev.Count, &prev.State, &prev.AttemptedAt, &prev.Generation); err != nil {
				return fmt.Errorf("reading CM detection attempt for recording %d: %w", recordingID, err)
			}
			next = nextCMAttemptRow(&prev, generation, maxAttempts, now)
			if _, err := tx.ExecContext(ctx, `
UPDATE recording_cm_attempts SET state = ?, attempt_count = ?, stage = ?, error = ?, attempted_at = ?, policy_generation = ?
WHERE recording_id = ?`, next.State, next.Count, next.Stage, next.Error, next.AttemptedAt, next.Generation, recordingID); err != nil {
				return fmt.Errorf("starting CM detection attempt for recording %d: %w", recordingID, err)
			}
		}
		attempt = store.CMDetectionAttempt{
			RecordingID:      recordingID,
			Station:          store.CMStationRef{NetworkID: item.NetworkID, ServiceID: item.ServiceID},
			AttemptCount:     next.Count,
			PolicyGeneration: next.Generation,
		}
		shouldRun = next.State == "running"
		return nil
	})
	if err != nil {
		return store.CMDetectionAttempt{}, false, err
	}
	return attempt, shouldRun, nil
}

// FailAttempt は一致する running 試行を失敗状態にし、更新したかを返す。
func (s *Analysis) FailAttempt(ctx context.Context, attempt store.CMDetectionAttempt, state string, stage *string, message string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE recording_cm_attempts SET state = ?, stage = ?, error = ?
WHERE recording_id = ? AND attempt_count = ? AND policy_generation = ? AND state = 'running'`,
		state, stage, message, attempt.RecordingID, attempt.AttemptCount, attempt.PolicyGeneration)
	if err != nil {
		return false, fmt.Errorf("marking CM detection attempt %d for recording %d as %s: %w", attempt.AttemptCount, attempt.RecordingID, state, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting CM detection attempt update for recording %d: %w", attempt.RecordingID, err)
	}
	return n > 0, nil
}

// CancelAttempt は一致する2回目以降の running 試行を予算を消費しない状態に戻す。
func (s *Analysis) CancelAttempt(ctx context.Context, attempt store.CMDetectionAttempt) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE recording_cm_attempts SET state = 'retrying', attempt_count = ? - 1, stage = NULL, error = NULL
WHERE recording_id = ? AND attempt_count = ? AND attempt_count > 1 AND policy_generation = ? AND state = 'running'`,
		attempt.AttemptCount, attempt.RecordingID, attempt.AttemptCount, attempt.PolicyGeneration)
	if err != nil {
		return fmt.Errorf("canceling CM detection attempt %d for recording %d: %w", attempt.AttemptCount, attempt.RecordingID, err)
	}
	return nil
}

// AbortAttempt は一致する初回の running 試行を削除する。
func (s *Analysis) AbortAttempt(ctx context.Context, attempt store.CMDetectionAttempt) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM recording_cm_attempts
WHERE recording_id = ? AND attempt_count = ? AND policy_generation = ? AND state = 'running'`,
		attempt.RecordingID, attempt.AttemptCount, attempt.PolicyGeneration)
	if err != nil {
		return fmt.Errorf("aborting CM detection attempt %d for recording %d: %w", attempt.AttemptCount, attempt.RecordingID, err)
	}
	return nil
}

// normalizeRanges は区間を start_ms 順に並べ、空の区間を捨て、重なる・接する区間を 1 つにまとめる。
// Postgres の int8multirange の正規化と同じ（pgstore は入力をこの形にしてから格納する）。
func normalizeRanges(ranges []chapters.Range) []chapters.Range {
	sorted := make([]chapters.Range, 0, len(ranges))
	for _, r := range ranges {
		if r.EndMs > r.StartMs {
			sorted = append(sorted, r)
		}
	}
	sortRanges(sorted)
	out := make([]chapters.Range, 0, len(sorted))
	for _, r := range sorted {
		if n := len(out); n > 0 && r.StartMs <= out[n-1].EndMs {
			if r.EndMs > out[n-1].EndMs {
				out[n-1].EndMs = r.EndMs
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// discardCMAttempt は録画の試行の行を消す。SaveDetection が結果を保存せずに試行を終えるときに使う。
func discardCMAttempt(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recording_cm_attempts WHERE recording_id = ?`, recordingID); err != nil {
		return fmt.Errorf("discarding CM detection attempt for recording %d: %w", recordingID, err)
	}
	return nil
}

// SaveDetection は一致する現在世代の試行について、正規化した CM 区間と任意のレビュー材料を保存する。
// 局、録画、試行の順で行を排他で取る（SaveLearnedLogo は局、試行の順。録画の行は取らない）。
// ごみ箱の録画でも desired でなければ試行を消すので、録画の行は lockRecordingState で取る。
// 局ロゴの版は、解析をロゴありで始めた試行（LogoLearnedAt が非 nil）だけ照合する。
// 行を取った後の状態で判定し直す。
func (s *Analysis) SaveDetection(ctx context.Context, attempt store.CMDetectionAttempt, ranges []chapters.Range, review store.CMDetectionMaterial) (bool, error) {
	var saved bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, attempt.Station); err != nil {
			return fmt.Errorf("locking station logo state for CM result: %w", err)
		}
		if err := lockRecordingState(ctx, tx, attempt.RecordingID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("locking recording %d for CM result: %w", attempt.RecordingID, err)
		}
		current, err := lockRunningAttempt(ctx, tx, attempt)
		if err != nil || !current {
			return err
		}
		desired, err := isDetectionDesired(ctx, tx, attempt.RecordingID)
		if err != nil {
			return fmt.Errorf("rechecking CM detection policy for recording %d: %w", attempt.RecordingID, err)
		}
		if !desired {
			return discardCMAttempt(ctx, tx, attempt.RecordingID)
		}
		if attempt.LogoLearnedAt != nil {
			var learnedAt time.Time
			err := tx.QueryRowContext(ctx, `SELECT learned_at FROM cm_logos WHERE network_id = ? AND service_id = ?`,
				attempt.Station.NetworkID, attempt.Station.ServiceID).Scan(&learnedAt)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !learnedAt.Equal(*attempt.LogoLearnedAt)) {
				return discardCMAttempt(ctx, tx, attempt.RecordingID)
			}
			if err != nil {
				return fmt.Errorf("checking station logo version for recording %d: %w", attempt.RecordingID, err)
			}
		}
		if review.Elements == nil {
			review.Elements = make([]chapters.CMElement, 0)
		}
		cmJSON, err := json.Marshal(normalizeRanges(ranges))
		if err != nil {
			return fmt.Errorf("encoding CM ranges: %w", err)
		}
		logoJSON, err := json.Marshal(normalizeRanges(review.LogoRanges))
		if err != nil {
			return fmt.Errorf("encoding logo ranges: %w", err)
		}
		elementsJSON, err := json.Marshal(review.Elements)
		if err != nil {
			return fmt.Errorf("encoding CM structure elements: %w", err)
		}
		// 結果の行は録画 1 件に 1 つ（主キーだけが一意）なので、ON DUPLICATE KEY UPDATE が狙う衝突以外で発火しない。
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_cm_detections (recording_id, cm_ranges, logo_ranges, structure_elements, detected_at)
VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE cm_ranges = incoming.cm_ranges, logo_ranges = incoming.logo_ranges,
    structure_elements = incoming.structure_elements, detected_at = incoming.detected_at`,
			attempt.RecordingID, string(cmJSON), string(logoJSON), string(elementsJSON)); err != nil {
			return fmt.Errorf("saving CM detection result for recording %d: %w", attempt.RecordingID, err)
		}
		if err := discardCMAttempt(ctx, tx, attempt.RecordingID); err != nil {
			return err
		}
		saved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return saved, nil
}

// SaveLearnedLogo は局の枠版と試行が一致する場合にロゴを保存し、保存したかを返す。
// 局、試行の順で行を排他で取る。局にロゴが既にあるか、枠の版が違えば保存しない。
func (s *Analysis) SaveLearnedLogo(ctx context.Context, attempt store.CMDetectionAttempt, logo store.CMDetectionLogo, observedAreaVersion *time.Time) (bool, error) {
	var saved bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockCMStation(ctx, tx, attempt.Station); err != nil {
			return fmt.Errorf("locking station logo state for recording %d: %w", attempt.RecordingID, err)
		}
		current, err := lockRunningAttempt(ctx, tx, attempt)
		if err != nil || !current {
			return err
		}
		var hasLogo bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM cm_logos WHERE network_id = ? AND service_id = ?)`,
			attempt.Station.NetworkID, attempt.Station.ServiceID).Scan(&hasLogo); err != nil {
			return fmt.Errorf("checking existing station logo for recording %d: %w", attempt.RecordingID, err)
		}
		if hasLogo {
			return nil
		}
		area, err := loadLogoArea(ctx, tx, attempt.Station)
		hasArea := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loading logo area for recording %d: %w", attempt.RecordingID, err)
		}
		// 枠が無いまま始まった解析と、枠の版が同じままの解析だけ保存する。
		if hasArea != (observedAreaVersion != nil) || (hasArea && !area.UpdatedAt.Equal(*observedAreaVersion)) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cm_logos (network_id, service_id, lgd, preview_png, learned_from, coded_width, coded_height)
VALUES (?, ?, ?, ?, ?, ?, ?)`, attempt.Station.NetworkID, attempt.Station.ServiceID, logo.LGD, logo.PreviewPNG,
			attempt.RecordingID, logo.CodedWidth, logo.CodedHeight); err != nil {
			return fmt.Errorf("saving learned station logo for recording %d: %w", attempt.RecordingID, err)
		}
		saved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return saved, nil
}

// RetryDetection は結果と試行を消し、原本がある場合に再試行情報を返す。
func (s *Analysis) RetryDetection(ctx context.Context, recordingID int64) (store.CMDetectionRetry, error) {
	var result store.CMDetectionRetry
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var purged bool
		err := tx.QueryRowContext(ctx, `SELECT purged_at IS NOT NULL FROM recordings WHERE id = ?`, recordingID).Scan(&purged)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && purged) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("loading recording %d for CM detection retry: %w", recordingID, err)
		}
		active, err := s.hasPresentOriginal(ctx, tx, recordingID)
		if err != nil {
			return fmt.Errorf("checking active original for recording %d: %w", recordingID, err)
		}
		if !active {
			result = store.CMDetectionRetry{ActiveOriginal: false}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_cm_detections WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("clearing CM detection result for recording %d: %w", recordingID, err)
		}
		if err := discardCMAttempt(ctx, tx, recordingID); err != nil {
			return err
		}
		desired, err := isDetectionDesired(ctx, tx, recordingID)
		if err != nil {
			return fmt.Errorf("checking CM detection policy for recording %d: %w", recordingID, err)
		}
		result = store.CMDetectionRetry{ActiveOriginal: true, Desired: desired}
		if desired {
			result.RecordingDurationMs, err = recordingDuration(ctx, tx, recordingID)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("getting recording %d duration for CM detection: %w", recordingID, err)
			}
		}
		return nil
	})
	if err != nil {
		return store.CMDetectionRetry{}, err
	}
	return result, nil
}

// DeleteStaleAttempts は現在の方針世代でない CM 検出試行を削除する。
// current_cm_attempts は recording_cm_attempts を読む view なので、削除の副問い合わせには使わず
// LEFT JOIN で結ぶ。NOT EXISTS で view を参照すると、MySQL 9.7 は 1443（The definition of table ... prevents operation DELETE）で拒む。
func (s *Analysis) DeleteStaleAttempts(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE ca FROM recording_cm_attempts ca
LEFT JOIN current_cm_attempts cur ON cur.recording_id = ca.recording_id
WHERE cur.recording_id IS NULL`); err != nil {
		return fmt.Errorf("clearing stale CM detection attempts: %w", err)
	}
	return nil
}

// ListMissingDetections は desired な CM 検出を recording ID 順に先頭から limit 件返す。
func (s *Analysis) ListMissingDetections(ctx context.Context, limit int32) ([]store.CMDetectionJob, error) {
	jobs, err := queryRows(ctx, s.db, "missing CM detections", `
SELECT d.recording_id, `+recordingDurationSQL+`
FROM cm_detection_desired d
JOIN recordings r ON r.id = d.recording_id
ORDER BY d.recording_id
LIMIT ?`, func(rows *sql.Rows) (store.CMDetectionJob, error) {
		var job store.CMDetectionJob
		err := rows.Scan(&job.RecordingID, &job.RecordingDurationMs)
		return job, err
	}, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing CM detections: %w", err)
	}
	return jobs, nil
}

// runningLogoCandidate は running の局候補の、締切判定に要る値。
type runningLogoCandidate struct {
	networkID, serviceID, recordingID int64
	areaUpdatedAt, attemptedAt        time.Time
}

// FailOrphanLogoCandidates は worker の締切と recoveryGrace を過ぎた running 候補を失敗にする。
// 締切と attempted_at の比較には、同じ tx で読んだ DB の時刻を使う。局の行は取らない（pgstore と同じ）。
func (s *Analysis) FailOrphanLogoCandidates(ctx context.Context, recoveryGrace time.Duration, timeoutFor func(int64) time.Duration) error {
	if timeoutFor == nil {
		return fmt.Errorf("CM logo candidate timeout function is nil")
	}
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		candidates, err := queryRows(ctx, tx, "running CM logo candidates", `
SELECT network_id, service_id, COALESCE(recording_id, 0), observed_area_updated_at, attempted_at
FROM cm_logo_candidates WHERE state = 'running'
ORDER BY network_id, service_id`, func(rows *sql.Rows) (runningLogoCandidate, error) {
			var c runningLogoCandidate
			err := rows.Scan(&c.networkID, &c.serviceID, &c.recordingID, &c.areaUpdatedAt, &c.attemptedAt)
			return c, err
		})
		if err != nil {
			return fmt.Errorf("listing running CM logo candidates: %w", err)
		}
		for _, c := range candidates {
			durationMs, err := recordingDuration(ctx, tx, c.recordingID)
			if errors.Is(err, sql.ErrNoRows) {
				// 録画が消えた候補は実行側の締切を引けない。結果は不要で、遅い書き込みは版照合で拒否される。
				durationMs = 0
			} else if err != nil {
				return fmt.Errorf("getting recording %d duration for orphan CM logo candidate: %w", c.recordingID, err)
			}
			deadline := c.attemptedAt.Add(timeoutFor(durationMs)).Add(recoveryGrace)
			if !now.After(deadline) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE cm_logo_candidates
SET state = 'failed', stage = 'stopped', error = 'CM logo candidate job ended without recording a result'
WHERE network_id = ? AND service_id = ? AND COALESCE(recording_id, 0) = ? AND state = 'running'
  AND observed_area_updated_at = ? AND attempted_at = ?`,
				c.networkID, c.serviceID, c.recordingID, c.areaUpdatedAt, c.attemptedAt); err != nil {
				return fmt.Errorf("failing orphan CM logo candidate for network %d service %d: %w", c.networkID, c.serviceID, err)
			}
		}
		return nil
	})
}
