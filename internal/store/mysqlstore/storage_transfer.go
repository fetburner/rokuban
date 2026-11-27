package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.StorageTransfer         = (*Media)(nil)
	_ store.TransferBacklogReader   = (*Media)(nil)
	_ store.IngestStoreChooser      = (*Media)(nil)
	_ store.IngestWriterStateReader = (*Media)(nil)
)

// desiredMediaStoreSQL は desired_media_store の MySQL 版 SQL 断片を返す。
// rules, derived の順に ? を使う。site と endAt は読み手の列式を渡す。
func desiredMediaStoreSQL(kind, site, endAt string) string {
	return `CASE
    WHEN ` + kind + ` IS NULL THEN NULL
    WHEN ` + kind + ` = 'original' THEN (
        SELECT JSON_UNQUOTE(JSON_EXTRACT(rule_row.rule, '$.store'))
        FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (
            ord FOR ORDINALITY,
            rule JSON PATH '$'
        )) AS rule_row
        WHERE (JSON_UNQUOTE(JSON_EXTRACT(rule_row.rule, '$.match.site')) IS NULL
               OR JSON_UNQUOTE(JSON_EXTRACT(rule_row.rule, '$.match.site')) = ` + site + `)
          AND (JSON_EXTRACT(rule_row.rule, '$.match.older_than_us') IS NULL
               OR (` + endAt + ` IS NOT NULL
                   AND ` + endAt + ` < TIMESTAMPADD(MICROSECOND,
                       -CAST(JSON_EXTRACT(rule_row.rule, '$.match.older_than_us') AS SIGNED), NOW(6))))
        ORDER BY rule_row.ord
        LIMIT 1
    )
    ELSE ?
END`
}

// transferPlacementCTE は desired な置き場と転送先を一度評価する CTE を返す。
// 引数は original_rules, derived_store, fs_stores JSON, derived_store の順。
func transferPlacementCTE() string {
	return `WITH desired AS (
    SELECT a.id, a.recording_id, a.size_bytes, a.kind, a.sha256,
           a.derived_generation AS generation, r.site, r.deleted_at,
           ` + desiredMediaStoreSQL("a.kind", "r.site", "TIMESTAMPADD(MICROSECOND, r.program_duration_ms * 1000, r.program_start_at)") + ` AS desired_store
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
), placements AS (
    SELECT desired.*,
           CASE WHEN desired.deleted_at IS NULL
                  AND (desired.desired_store IN (
                      SELECT fs.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (
                          store VARCHAR(64) PATH '$'
                      )) AS fs
                  ) OR (desired.kind <> 'original' AND desired.desired_store = ?))
                THEN desired.desired_store END AS destination_store
    FROM desired
)
`
}

type transferQueryBuilder struct {
	query strings.Builder
	args  []any
}

func (b *transferQueryBuilder) appendSQL(query string, args ...any) {
	b.query.WriteString(query)
	b.args = append(b.args, args...)
}

func (b *transferQueryBuilder) appendPlacement(policy store.StoragePolicy, fsStores []string) ([]byte, error) {
	rules, err := originalRulesJSON(policy)
	if err != nil {
		return nil, err
	}
	fsJSON, err := json.Marshal(fsStores)
	if err != nil {
		return nil, fmt.Errorf("encoding filesystem stores: %w", err)
	}
	b.appendSQL(transferPlacementCTE(), rules, policy.DerivedStore(), fsJSON, policy.DerivedStore())
	return fsJSON, nil
}

func (b *transferQueryBuilder) appendCandidate(sourceStores []byte, derivedStore string) {
	b.appendSQL(storageTransferCandidateSQL, append([]any{sourceStores}, untilEncodedArgs(derivedStore)...)...)
}

func (b *transferQueryBuilder) appendPendingCopyObsolete(derivedStore string) {
	b.appendSQL(storageTransferPendingCopyObsoleteSQL, untilEncodedArgs(derivedStore)...)
}

func (b *transferQueryBuilder) String() string {
	return b.query.String()
}

// storageTransferCandidateSQL は、present なソースがあり desired copy が無い転送候補を表す。
// 引数は source stores JSON、その後 until_encoded_deletable_originals の desired store 4 個。
const storageTransferCandidateSQL = `
a.destination_store IS NOT NULL
AND NOT EXISTS (
    SELECT 1 FROM active_present_media_asset_copies target
    WHERE target.media_asset_id = a.id AND target.store = a.destination_store
)
AND EXISTS (
    SELECT 1 FROM active_present_media_asset_copies source
    WHERE source.media_asset_id = a.id
      AND source.store IN (
          SELECT fs.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (
              store VARCHAR(64) PATH '$'
          )) AS fs
      )
      AND source.store <> a.destination_store
)
AND NOT (a.kind = 'original' AND ` + untilEncodedDeletableSQL + `)
`

// storageTransferAttemptCountSQL は、current generation の destination copy の失敗数を返す。
const storageTransferAttemptCountSQL = `COALESCE((
    SELECT MAX(attempt.attempt_count)
    FROM media_asset_copies target
    JOIN media_asset_transfer_attempts attempt ON attempt.copy_id = target.id
    WHERE target.media_asset_id = a.id
      AND target.store = a.destination_store
      AND target.generation = a.generation
      AND target.state <> 'deleted'
), 0)`

// storageTransferPendingCopyObsoleteSQL は不要になった pending copy の述語である。
// 引数は until_encoded_deletable_originals の desired store 4 個。
const storageTransferPendingCopyObsoleteSQL = `
c.state = 'pending'
AND (
    c.generation <> a.generation
    OR NOT (a.destination_store <=> c.store)
    OR (a.kind = 'original' AND ` + untilEncodedDeletableSQL + `)
)
`

func transferCopyCandidate(rows *sql.Rows) (store.MediaAssetCopyDeleteCandidate, error) {
	var out store.MediaAssetCopyDeleteCandidate
	err := rows.Scan(&out.CopyID, &out.MediaAssetID, &out.Store, &out.RelPath, &out.State,
		&out.SizeBytes, &out.Kind, &out.Generation)
	return out, err
}

// ListActiveDerivedCopiesInNonDerivedRemote は設定済み遠隔置き場の current-generation コピーを数える。
func (s *Media) ListActiveDerivedCopiesInNonDerivedRemote(ctx context.Context, remoteStores []string) ([]store.DerivedRemoteCopyCount, error) {
	filter, args := inSQL("c.store", remoteStores)
	if len(remoteStores) == 0 {
		return []store.DerivedRemoteCopyCount{}, nil
	}
	rows, err := queryRows(ctx, s.db, "active derived copies in non-derived remote stores", `
SELECT c.store, COUNT(*)
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE `+filter+` AND c.store <> ? AND c.state = 'active'
  AND c.generation = a.derived_generation AND a.kind <> 'original'
GROUP BY c.store ORDER BY c.store`, func(rows *sql.Rows) (store.DerivedRemoteCopyCount, error) {
		var row store.DerivedRemoteCopyCount
		err := rows.Scan(&row.Store, &row.Copies)
		return row, err
	}, append(args, s.policy.DerivedStore())...)
	if err != nil {
		return nil, fmt.Errorf("listing active derived copies in non-derived remote stores: %w", err)
	}
	return rows, nil
}

// ListMediaAssetTransferCandidates は desired store に present なコピーがない論理資産を列挙する。
func (s *Media) ListMediaAssetTransferCandidates(ctx context.Context, attemptLimit, limit int32) ([]store.MediaAssetTransferCandidate, error) {
	var query transferQueryBuilder
	fsJSON, err := query.appendPlacement(s.policy, s.policy.FSStores())
	if err != nil {
		return nil, fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	query.appendSQL(`
SELECT a.id, a.desired_store,
       COALESCE((
           SELECT c.store
           FROM active_present_media_asset_copies source
           JOIN media_asset_copies c ON c.id = source.id
           WHERE source.media_asset_id = a.id
             AND source.store IN (SELECT fs.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (store VARCHAR(64) PATH '$')) fs)
           ORDER BY c.created_at DESC, c.id
           LIMIT 1
       ), '') AS source_store
FROM placements a
WHERE `, fsJSON)
	query.appendCandidate(fsJSON, s.policy.DerivedStore())
	query.appendSQL(`
  AND `+storageTransferAttemptCountSQL+` < ?
ORDER BY a.id
LIMIT ?`, attemptLimit, limit)
	rows, err := queryRows(ctx, s.db, "media asset transfer candidates", query.String(), func(rows *sql.Rows) (store.MediaAssetTransferCandidate, error) {
		var out store.MediaAssetTransferCandidate
		err := rows.Scan(&out.MediaAssetID, &out.Store, &out.SourceStore)
		return out, err
	}, query.args...)
	if err != nil {
		return nil, fmt.Errorf("listing media asset transfer candidates: %w", err)
	}
	return rows, nil
}

// CountMediaAssetTransferCandidates は転送待ちの件数とバイト数を返す。
func (s *Media) CountMediaAssetTransferCandidates(ctx context.Context, attemptLimit int32) (int64, int64, error) {
	var query transferQueryBuilder
	fsJSON, err := query.appendPlacement(s.policy, s.policy.FSStores())
	if err != nil {
		return 0, 0, fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	query.appendSQL(`
SELECT COUNT(*), COALESCE(SUM(a.size_bytes), 0)
FROM placements a
WHERE `)
	query.appendCandidate(fsJSON, s.policy.DerivedStore())
	query.appendSQL(`
  AND `+storageTransferAttemptCountSQL+` < ?`, attemptLimit)
	var count, bytes int64
	if err := s.db.QueryRowContext(ctx, query.String(), query.args...).Scan(&count, &bytes); err != nil {
		return 0, 0, fmt.Errorf("counting media asset transfer candidates: %w", err)
	}
	return count, bytes, nil
}

// CountMediaAssetTransferBacklogBySourceStore は転送元置き場ごとの転送候補数を返す。
func (s *Media) CountMediaAssetTransferBacklogBySourceStore(ctx context.Context, sourceStores []string, attemptLimit int32) (map[string]int64, error) {
	var query transferQueryBuilder
	fsJSON, err := query.appendPlacement(s.policy, s.policy.FSStores())
	if err != nil {
		return nil, fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	sourceJSON, err := json.Marshal(sourceStores)
	if err != nil {
		return nil, fmt.Errorf("encoding transfer source stores: %w", err)
	}
	query.appendSQL(`
SELECT source.store, COUNT(DISTINCT a.id)
FROM placements a
JOIN active_media_asset_copies source ON source.media_asset_id = a.id
WHERE source.store IN (SELECT s.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (store VARCHAR(64) PATH '$')) s)
  AND `, sourceJSON)
	query.appendCandidate(fsJSON, s.policy.DerivedStore())
	query.appendSQL(`
  AND `+storageTransferAttemptCountSQL+` < ?
GROUP BY source.store`, attemptLimit)
	rows, err := queryRows(ctx, s.db, "transfer backlog by source store", query.String(), func(rows *sql.Rows) (struct {
		store string
		count int64
	}, error) {
		var row struct {
			store string
			count int64
		}
		err := rows.Scan(&row.store, &row.count)
		return row, err
	}, query.args...)
	if err != nil {
		return nil, fmt.Errorf("counting media asset transfer backlog by source store: %w", err)
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.store] = row.count
	}
	return counts, nil
}

// CountMediaAssetTransferExhausted は失敗上限に達した転送候補数を返す。
func (s *Media) CountMediaAssetTransferExhausted(ctx context.Context, attemptLimit int32) (int64, error) {
	var query transferQueryBuilder
	fsJSON, err := query.appendPlacement(s.policy, s.policy.FSStores())
	if err != nil {
		return 0, fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	query.appendSQL(`
SELECT COUNT(*) FROM placements a
WHERE `)
	query.appendCandidate(fsJSON, s.policy.DerivedStore())
	query.appendSQL(`
  AND `+storageTransferAttemptCountSQL+` >= ?`, attemptLimit)
	var count int64
	if err := s.db.QueryRowContext(ctx, query.String(), query.args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting exhausted media asset transfers: %w", err)
	}
	return count, nil
}

// ClearObsoleteMediaAssetTransferAttempts は不要になった pending copy の失敗行を消す。
func (s *Media) ClearObsoleteMediaAssetTransferAttempts(ctx context.Context) error {
	var query transferQueryBuilder
	if _, err := query.appendPlacement(s.policy, s.policy.FSStores()); err != nil {
		return fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	query.appendSQL(`
DELETE attempt
FROM media_asset_transfer_attempts attempt
JOIN media_asset_copies c ON c.id = attempt.copy_id
JOIN placements a ON a.id = c.media_asset_id
WHERE `)
	query.appendPendingCopyObsolete(s.policy.DerivedStore())
	if err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, query.String(), query.args...); err != nil {
			return fmt.Errorf("deleting obsolete media asset transfer attempts: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("clearing obsolete media asset transfer attempts: %w", err)
	}
	return nil
}

const transferBandwidthLockPrefix = "rk-transfer:"

// transferBandwidthLockName は MySQL サーバー全体で一意になる 64 文字以内のロック名を作る。
// GET_LOCK 名は DB 間で共有されるため、DATABASE() と source store の両方を SHA-256 に含める。
func transferBandwidthLockName(ctx context.Context, conn *sql.Conn, sourceStore string) (string, error) {
	var database sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
		return "", fmt.Errorf("reading current database for transfer lock: %w", err)
	}
	if !database.Valid || database.String == "" {
		return "", errors.New("current database is empty for transfer lock")
	}
	sum := sha256.Sum256([]byte("rokuban-storage-transfer\x00" + database.String + "\x00" + sourceStore))
	// 12 文字の prefix と 52 桁の hex digest で MySQL の 64 文字上限に収める。
	return transferBandwidthLockPrefix + hex.EncodeToString(sum[:26]), nil
}

// discardMySQLConn は driver.ErrBadConn を Conn.Raw に返し、物理接続を pool から捨てる。
// database/sql.Conn.Raw の契約では callback が driver.ErrBadConn を返すと、その接続は再利用されない。
func discardMySQLConn(conn *sql.Conn) error {
	rawErr := conn.Raw(func(any) error { return driver.ErrBadConn })
	if rawErr != nil && !errors.Is(rawErr, driver.ErrBadConn) {
		return fmt.Errorf("discarding MySQL connection with Conn.Raw: %w", rawErr)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("closing discarded MySQL connection: %w", err)
	}
	return nil
}

// AcquireTransferSourceBandwidth は転送元ごとの advisory lock を専用接続で保持する。
func (s *Media) AcquireTransferSourceBandwidth(ctx context.Context, sourceStore string) (store.TransferBandwidthLease, bool, error) {
	if sourceStore == "" {
		return nil, false, errors.New("transfer source store is empty")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring transfer bandwidth connection: %w", err)
	}
	name, err := transferBandwidthLockName(ctx, conn, sourceStore)
	if err != nil {
		return nil, false, errors.Join(err, conn.Close())
	}
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 0)`, name).Scan(&acquired); err != nil {
		return nil, false, errors.Join(fmt.Errorf("acquiring transfer bandwidth lock for %q: %w", sourceStore, err), discardMySQLConn(conn))
	}
	if !acquired.Valid {
		return nil, false, errors.Join(fmt.Errorf("acquiring transfer bandwidth lock for %q: GET_LOCK returned NULL", sourceStore), discardMySQLConn(conn))
	}
	if acquired.Int64 != 1 {
		if err := conn.Close(); err != nil {
			return nil, false, fmt.Errorf("returning busy transfer lock connection: %w", err)
		}
		return nil, false, nil
	}
	return &transferBandwidthLease{conn: conn, name: name}, true, nil
}

type transferBandwidthLease struct {
	conn *sql.Conn
	name string
	once sync.Once
	err  error
}

// Release は GET_LOCK を解放して接続を pool に戻す。失敗した接続は Conn.Raw の契約で破棄する。
func (l *transferBandwidthLease) Release(ctx context.Context) error {
	l.once.Do(func() {
		var released sql.NullInt64
		if err := l.conn.QueryRowContext(ctx, `SELECT RELEASE_LOCK(?)`, l.name).Scan(&released); err != nil {
			l.err = errors.Join(fmt.Errorf("releasing transfer bandwidth lock: %w", err), discardMySQLConn(l.conn))
			return
		}
		if !released.Valid || released.Int64 != 1 {
			l.err = errors.Join(fmt.Errorf("releasing transfer bandwidth lock %q: RELEASE_LOCK returned %v", l.name, released), discardMySQLConn(l.conn))
			return
		}
		if err := l.conn.Close(); err != nil {
			l.err = fmt.Errorf("returning released transfer bandwidth connection: %w", err)
		}
	})
	return l.err
}

var _ store.TransferBandwidthLease = (*transferBandwidthLease)(nil)

// DesiredOriginalStoreAtIngest は年齢 0 で一致する原本の置き場を先勝ちで返す。
func (s *Media) DesiredOriginalStoreAtIngest(ctx context.Context, site string) (string, error) {
	rules, err := originalRulesJSON(s.policy)
	if err != nil {
		return "", fmt.Errorf("encoding original placement policy: %w", err)
	}
	query := `SELECT COALESCE(` + desiredMediaStoreSQL("'original'", "?", "NOW(6)") + `, '')`
	var name string
	if err := s.db.QueryRowContext(ctx, query, rules, site, s.policy.DerivedStore()).Scan(&name); err != nil {
		return "", fmt.Errorf("evaluating original store rules for site %q: %w", site, err)
	}
	return name, nil
}

// GetIngestWriterState は mirakc record に対応する checkpoint と表示中の ingest temp を返す。
func (s *Media) GetIngestWriterState(ctx context.Context, site, recordID string) (store.IngestWriterState, bool, error) {
	var out store.IngestWriterState
	var hasProgress bool
	err := s.db.QueryRowContext(ctx, `
SELECT checkpoint.recording_id, checkpoint.writer_generation,
       COALESCE(progress.temp_rel_path, ''), progress.recording_id IS NOT NULL
FROM record_sync sync
JOIN recording_ingest_checkpoints checkpoint ON checkpoint.recording_id = sync.recording_id
LEFT JOIN recording_ingest_progress progress ON progress.recording_id = checkpoint.recording_id
WHERE sync.site = ? AND sync.record_id = ?`, site, recordID).
		Scan(&out.RecordingID, &out.WriterGeneration, &out.TempRelPath, &hasProgress)
	if errors.Is(err, sql.ErrNoRows) {
		return store.IngestWriterState{}, false, nil
	}
	if err != nil {
		return store.IngestWriterState{}, false, fmt.Errorf("getting ingest writer state for %s/%s: %w", site, recordID, err)
	}
	out.HasProgress = hasProgress
	return out, true, nil
}

// PrepareMediaAssetTransfer は行をロックし、必要な宛先コピーを pending に予約する。
func (s *Media) PrepareMediaAssetTransfer(ctx context.Context, mediaAssetID int64, targetStore string, fsStores, sourceStores, mountedStores []string, attemptLimit int32) (store.MediaAssetTransfer, bool, error) {
	fsJSON, err := json.Marshal(fsStores)
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("encoding transfer filesystem stores: %w", err)
	}
	sourceJSON, err := json.Marshal(sourceStores)
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("encoding transfer source stores: %w", err)
	}
	mountedJSON, err := json.Marshal(mountedStores)
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("encoding mounted transfer stores: %w", err)
	}
	var transfer store.MediaAssetTransfer
	prepared := false
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE id = ? FOR UPDATE`, mediaAssetID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking media asset %d before transfer reservation: %w", mediaAssetID, err)
		}

		var candidateQuery transferQueryBuilder
		// PrepareMediaAssetTransfer は候補の宛先判定に呼び出し側が認識するソース置き場を使う。
		if _, err := candidateQuery.appendPlacement(s.policy, sourceStores); err != nil {
			return fmt.Errorf("encoding media asset transfer policy: %w", err)
		}
		candidateQuery.appendSQL(`
		SELECT a.id
		FROM placements a
		WHERE a.id = ? AND a.destination_store = ?
		  AND `, mediaAssetID, targetStore)
		candidateQuery.appendCandidate(sourceJSON, s.policy.DerivedStore())
		candidateQuery.appendSQL(`
		  AND `+storageTransferAttemptCountSQL+` < ?`, attemptLimit)
		var eligible int64
		if err := tx.QueryRowContext(ctx, candidateQuery.String(), candidateQuery.args...).Scan(&eligible); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("checking media asset %d transfer eligibility: %w", mediaAssetID, err)
		}

		var asset struct {
			recordingID int64
			sizeBytes   int64
			kind        string
			sha256      []byte
			generation  int64
		}
		if err := tx.QueryRowContext(ctx, `
SELECT a.recording_id, a.size_bytes, a.kind, a.sha256, a.derived_generation
FROM media_assets a WHERE a.id = ?`, mediaAssetID).
			Scan(&asset.recordingID, &asset.sizeBytes, &asset.kind, &asset.sha256, &asset.generation); err != nil {
			return fmt.Errorf("reading media asset %d for transfer: %w", mediaAssetID, err)
		}
		var source struct {
			copyID int64
			store  string
			path   string
		}
		err = tx.QueryRowContext(ctx, `
SELECT source.id, source.store, copy.rel_path
FROM active_present_media_asset_copies source
JOIN media_asset_copies copy ON copy.id = source.id
WHERE source.media_asset_id = ? AND source.store <> ?
  AND source.store IN (SELECT src.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (store VARCHAR(64) PATH '$')) src)
  AND (? = 'original' OR source.store IN (
      SELECT fs.store FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (store VARCHAR(64) PATH '$')) fs
  ))
ORDER BY EXISTS (
    SELECT 1 FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (store VARCHAR(64) PATH '$')) mounted
    WHERE mounted.store = source.store
) DESC, copy.created_at DESC, copy.id
LIMIT 1`, mediaAssetID, targetStore, sourceJSON, asset.kind, fsJSON, mountedJSON).
			Scan(&source.copyID, &source.store, &source.path)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("selecting source for media asset %d transfer: %w", mediaAssetID, err)
		}

		var copyID int64
		var relPath, state string
		err = tx.QueryRowContext(ctx, `
SELECT id, rel_path, state FROM media_asset_copies
WHERE media_asset_id = ? AND store = ? AND generation = ? AND state <> 'deleted'
FOR UPDATE`, mediaAssetID, targetStore, asset.generation).Scan(&copyID, &relPath, &state)
		if errors.Is(err, sql.ErrNoRows) {
			token := uuid.NewString()
			relPath = transferObjectPath(source.path, token)
			res, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, ?, ?, ?, 'pending')`, mediaAssetID, targetStore, relPath, asset.generation)
			if err != nil {
				return fmt.Errorf("creating pending media asset transfer copy: %w", translateError(err))
			}
			copyID, err = res.LastInsertId()
			if err != nil {
				return fmt.Errorf("reading created transfer copy ID: %w", err)
			}
			state = "pending"
		} else if err != nil {
			return fmt.Errorf("locking target copy for media asset %d transfer: %w", mediaAssetID, err)
		} else if state == "active" {
			var missing bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM missing_media_assets WHERE media_asset_copy_id = ?)`, copyID).Scan(&missing); err != nil {
				return fmt.Errorf("checking missing target copy %d: %w", copyID, err)
			}
			if !missing {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copies SET state = 'pending', deleted_at = NULL WHERE id = ?`, copyID); err != nil {
				return fmt.Errorf("reserving missing target copy %d: %w", copyID, err)
			}
			state = "pending"
		} else if state != "pending" {
			return nil
		}
		if state != "pending" {
			return nil
		}
		transfer = store.MediaAssetTransfer{
			MediaAssetID: mediaAssetID, RecordingID: asset.recordingID, Generation: asset.generation,
			RelPath: relPath, SizeBytes: asset.sizeBytes, Kind: asset.kind, SHA256: asset.sha256,
			DesiredStore: targetStore, SourceStore: source.store, SourceRelPath: source.path,
			SourceCopyID: source.copyID, CopyID: copyID,
		}
		prepared = true
		return nil
	})
	if err != nil {
		return store.MediaAssetTransfer{}, false, err
	}
	return transfer, prepared, nil
}

func transferObjectPath(sourcePath, token string) string {
	lastSlash := strings.LastIndexByte(sourcePath, '/')
	prefix := sourcePath[:lastSlash+1]
	lastDot := strings.LastIndexByte(sourcePath, '.')
	if lastDot <= lastSlash {
		return prefix + token
	}
	return prefix + token + sourcePath[lastDot:]
}

// CompleteMediaAssetTransfer は policy と generation を再評価し、条件を満たすコピーを active にする。
func (s *Media) CompleteMediaAssetTransfer(ctx context.Context, transfer store.MediaAssetTransfer, targetStore string, sha256 []byte) (bool, string, error) {
	rules, err := originalRulesJSON(s.policy)
	if err != nil {
		return false, "", fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	var outcome string
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		current, initialOutcome, err := readMediaAssetTransferCompletionState(ctx, tx, transfer, targetStore, rules, s.policy.DerivedStore())
		if err != nil {
			return err
		}
		outcome = initialOutcome
		if outcome == "" {
			var activate bool
			outcome, activate, err = mediaAssetTransferCompletionOutcome(ctx, tx, current, transfer, targetStore, s.policy.DerivedStore(), sha256)
			if err != nil {
				return err
			}
			if activate {
				if err := activateMediaAssetTransfer(ctx, tx, transfer, targetStore, sha256); err != nil {
					return err
				}
				outcome = "activated"
			}
		}
		if outcome == "activated" || outcome == "already_active" {
			if err := clearCompletedMediaAssetTransfer(ctx, tx, transfer.CopyID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return outcome == "activated" || outcome == "already_active", outcome, nil
}

type mediaAssetTransferCompletionState struct {
	trashed    bool
	purged     bool
	kind       string
	generation int64
	sizeBytes  int64
	sha256     []byte
	desired    sql.NullString
	copyState  sql.NullString
}

func readMediaAssetTransferCompletionState(ctx context.Context, tx *sql.Tx, transfer store.MediaAssetTransfer, targetStore string, rules []byte, derivedStore string) (mediaAssetTransferCompletionState, string, error) {
	var state mediaAssetTransferCompletionState
	trashed, purged, err := lockRecordingFlags(ctx, tx, transfer.RecordingID)
	state.trashed, state.purged = trashed, purged
	if errors.Is(err, store.ErrNotFound) {
		return state, "stale", nil
	}
	if err != nil {
		return state, "", fmt.Errorf("locking recording %d before transfer completion: %w", transfer.RecordingID, err)
	}
	var assetLocked int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE id = ? AND recording_id = ? FOR UPDATE`, transfer.MediaAssetID, transfer.RecordingID).Scan(&assetLocked)
	if errors.Is(err, sql.ErrNoRows) {
		return state, "stale", nil
	}
	if err != nil {
		return state, "", fmt.Errorf("locking media asset %d before transfer completion: %w", transfer.MediaAssetID, err)
	}
	query := `SELECT a.kind, a.derived_generation, a.size_bytes, a.sha256,
` + desiredMediaStoreSQL("a.kind", "r.site", "TIMESTAMPADD(MICROSECOND, r.program_duration_ms * 1000, r.program_start_at)") + `
FROM media_assets a JOIN recordings r ON r.id = a.recording_id WHERE a.id = ?`
	if err := tx.QueryRowContext(ctx, query, rules, derivedStore, transfer.MediaAssetID).
		Scan(&state.kind, &state.generation, &state.sizeBytes, &state.sha256, &state.desired); err != nil {
		return state, "", fmt.Errorf("reading media asset %d for transfer completion: %w", transfer.MediaAssetID, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM media_asset_copies
WHERE id = ? AND media_asset_id = ? AND store = ?`, transfer.CopyID, transfer.MediaAssetID, targetStore).Scan(&state.copyState); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, "", fmt.Errorf("reading destination copy %d for transfer completion: %w", transfer.CopyID, err)
	}
	return state, "", nil
}

func mediaAssetTransferCompletionOutcome(ctx context.Context, tx *sql.Tx, current mediaAssetTransferCompletionState, transfer store.MediaAssetTransfer, targetStore, derivedStore string, sha256 []byte) (string, bool, error) {
	if current.copyState.Valid && current.copyState.String == "active" {
		return "already_active", false, nil
	}
	if current.sha256 != nil && !bytesEqual(current.sha256, sha256) {
		return "sha_mismatch", false, nil
	}
	if !current.trashed && !current.purged && current.copyState.Valid && current.copyState.String == "pending" &&
		current.generation == transfer.Generation && current.sizeBytes == transfer.SizeBytes &&
		current.desired.Valid && current.desired.String == targetStore {
		if current.kind == "original" {
			var untilEncoded bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM media_assets a WHERE a.id = ? AND `+untilEncodedDeletableSQL+`)`,
				append([]any{transfer.MediaAssetID}, untilEncodedArgs(derivedStore)...)...).Scan(&untilEncoded); err != nil {
				return "", false, fmt.Errorf("checking original retention before transfer completion: %w", err)
			}
			if untilEncoded {
				return "stale", false, nil
			}
		}
		return "", true, nil
	}
	return "stale", false, nil
}

func activateMediaAssetTransfer(ctx context.Context, tx *sql.Tx, transfer store.MediaAssetTransfer, targetStore string, sha256 []byte) error {
	now, err := transactionTime(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE media_assets SET sha256 = ? WHERE id = ?`, sha256, transfer.MediaAssetID); err != nil {
		return fmt.Errorf("recording transfer digest for media asset %d: %w", transfer.MediaAssetID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copies
SET state = 'active', created_at = ?, deleted_at = NULL
WHERE id = ? AND media_asset_id = ? AND store = ? AND generation = ? AND state = 'pending'`,
		now, transfer.CopyID, transfer.MediaAssetID, targetStore, transfer.Generation); err != nil {
		return fmt.Errorf("activating media asset %d transfer copy: %w", transfer.MediaAssetID, err)
	}
	for _, statement := range []string{
		`DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`,
		`DELETE FROM media_asset_transfer_attempts WHERE copy_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, transfer.CopyID); err != nil {
			return fmt.Errorf("clearing transfer state for copy %d: %w", transfer.CopyID, err)
		}
	}
	return nil
}

func clearCompletedMediaAssetTransfer(ctx context.Context, tx *sql.Tx, copyID int64) error {
	for _, statement := range []string{
		`DELETE FROM media_asset_transfer_objects WHERE copy_id = ?`,
		`DELETE FROM media_asset_copy_transfer_progress WHERE copy_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, copyID); err != nil {
			return fmt.Errorf("clearing completed transfer state for copy %d: %w", copyID, err)
		}
	}
	if err := upsertLoopMarker(ctx, tx, store.LoopStorageTransfer, store.StorageTransferScope); err != nil {
		return fmt.Errorf("upserting storage transfer marker for copy %d: %w", copyID, err)
	}
	return nil
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	return slices.Equal(left, right)
}

// RecordMediaAssetTransferFailure は転送失敗を destination copy ごとに記録する。
func (s *Media) RecordMediaAssetTransferFailure(ctx context.Context, copyID int64, message string) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_transfer_attempts (copy_id, attempt_count, last_error)
VALUES (?, 1, LEFT(?, 2048)) AS incoming
ON DUPLICATE KEY UPDATE attempt_count = media_asset_transfer_attempts.attempt_count + 1,
    last_error = incoming.last_error, updated_at = CURRENT_TIMESTAMP(6)`, copyID, message)
		if err != nil {
			return fmt.Errorf("upserting transfer failure for copy %d: %w", copyID, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording transfer failure for media asset copy %d: %w", copyID, err)
	}
	return nil
}

// MarkMediaAssetTransferObjectOwned は pending copy のために作成した key の所有を記録する。
// 同じ key の記録が既にあれば何もしない。Postgres の ON CONFLICT DO NOTHING に当たる。
// 主キー以外の一意キーを足すと、その衝突も no-op になるので、この形は使えなくなる。
func (s *Media) MarkMediaAssetTransferObjectOwned(ctx context.Context, copyID int64, storeName, relPath string) (store.TransferObjectOwnership, error) {
	var ownership store.TransferObjectOwnership
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT c.id FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE c.id = ? AND c.store = ? AND c.state = 'pending'
FOR UPDATE`, copyID, storeName).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			var state sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT state FROM media_asset_copies WHERE id = ? AND store = ?`, copyID, storeName).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && state.String == "deleted") {
				ownership = store.TransferObjectCopyRetired
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading transfer copy %d state after ownership record: %w", copyID, err)
			}
			ownership = store.TransferObjectCopyAdvanced
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking pending transfer copy %d to record object ownership: %w", copyID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO media_asset_transfer_objects (copy_id, rel_path)
VALUES (?, ?) ON DUPLICATE KEY UPDATE copy_id = copy_id`, copyID, relPath); err != nil {
			return fmt.Errorf("recording ownership of transfer object %q: %w", relPath, err)
		}
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM media_asset_transfer_objects object
JOIN media_asset_copies copy ON copy.id = object.copy_id
WHERE object.copy_id = ? AND copy.store = ? AND copy.state = 'pending' AND object.rel_path_key = UNHEX(SHA2(?, 256))
)`, copyID, storeName, relPath).Scan(&found); err != nil {
			return fmt.Errorf("checking ownership of transfer object %q: %w", relPath, err)
		}
		if !found {
			return fmt.Errorf("transfer object %q is missing from the ownership record after it was written", relPath)
		}
		ownership = store.TransferObjectOwned
		return nil
	})
	if err != nil {
		return 0, err
	}
	return ownership, nil
}

// ListMediaAssetTransferObjectsOwned は pending copy のために作成した key を返す。
func (s *Media) ListMediaAssetTransferObjectsOwned(ctx context.Context, copyID int64, storeName string) ([]string, error) {
	rows, err := queryRows(ctx, s.db, "owned transfer object paths", `
SELECT object.rel_path
FROM media_asset_transfer_objects object
JOIN media_asset_copies copy ON copy.id = object.copy_id
WHERE object.copy_id = ? AND copy.store = ? AND copy.state = 'pending'
ORDER BY object.rel_path`, func(rows *sql.Rows) (string, error) {
		var path string
		err := rows.Scan(&path)
		return path, err
	}, copyID, storeName)
	if err != nil {
		return nil, fmt.Errorf("listing owned transfer object keys for copy %d: %w", copyID, err)
	}
	return rows, nil
}

// DeleteMediaAssetTransferObjectOwned は削除済み key の ownership 記録を消す。
func (s *Media) DeleteMediaAssetTransferObjectOwned(ctx context.Context, copyID int64, storeName, relPath string) error {
	if err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE object FROM media_asset_transfer_objects object
JOIN media_asset_copies copy ON copy.id = object.copy_id
WHERE object.copy_id = ? AND copy.store = ? AND object.rel_path_key = UNHEX(SHA2(?, 256))`, copyID, storeName, relPath); err != nil {
			return fmt.Errorf("clearing ownership of transfer object %q: %w", relPath, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("clearing ownership of transfer object %q: %w", relPath, err)
	}
	return nil
}

// MarkMediaAssetCopyCorrupt は原本が present なときだけ派生コピーを deleting にする。
func (s *Media) MarkMediaAssetCopyCorrupt(ctx context.Context, recordingID, mediaAssetID, copyID int64, storeName string) (bool, error) {
	marked := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		trashed, purged, err := lockRecordingFlags(ctx, tx, recordingID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (trashed || purged)) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking recording %d before corrupt copy retirement: %w", recordingID, err)
		}
		var asset struct {
			kind       string
			generation int64
		}
		err = tx.QueryRowContext(ctx, `SELECT kind, derived_generation FROM media_assets WHERE id = ? AND recording_id = ? FOR UPDATE`, mediaAssetID, recordingID).
			Scan(&asset.kind, &asset.generation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking media asset %d before corrupt copy retirement: %w", mediaAssetID, err)
		}
		if asset.kind == "original" {
			return nil
		}
		var lockedCopy int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM media_asset_copies
WHERE id = ? AND media_asset_id = ? AND store = ? AND state = 'active' AND generation = ? FOR UPDATE`,
			copyID, mediaAssetID, storeName, asset.generation).Scan(&lockedCopy)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking corrupt media asset copy %d: %w", copyID, err)
		}
		var hasPresentOriginal bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM media_assets original
    JOIN active_present_media_asset_copies present ON present.media_asset_id = original.id
    WHERE original.recording_id = ? AND original.kind = 'original'
)`, recordingID).Scan(&hasPresentOriginal); err != nil {
			return fmt.Errorf("checking present original for recording %d: %w", recordingID, err)
		}
		if !hasPresentOriginal {
			return nil
		}
		// ロックした行は active で、deleting への遷移で値が変わるため 1 行に必ず反映される。
		res, err := tx.ExecContext(ctx, `UPDATE media_asset_copies SET state = 'deleting', deleted_at = NULL WHERE id = ?`, copyID)
		if err != nil {
			return fmt.Errorf("marking corrupt media asset %d copy in %q deleting: %w", mediaAssetID, storeName, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting corrupt copy retirements: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("marking corrupt media asset %d copy in %q deleting updated %d rows, want one", mediaAssetID, storeName, n)
		}
		marked = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}

// DiscardPendingMediaAssetCopy は転送失敗した pending copy を deleted にし、付随状態を消す。
func (s *Media) DiscardPendingMediaAssetCopy(ctx context.Context, copyID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM media_asset_copies WHERE id = ? FOR UPDATE`, copyID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "pending") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking pending media asset copy %d: %w", copyID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copies
SET state = 'deleted', deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, copyID); err != nil {
			return fmt.Errorf("discarding pending media asset copy %d: %w", copyID, err)
		}
		for _, statement := range []string{
			`DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`,
			`DELETE FROM media_asset_transfer_objects WHERE copy_id = ?`,
			`DELETE FROM media_asset_transfer_attempts WHERE copy_id = ?`,
			`DELETE FROM media_asset_copy_transfer_progress WHERE copy_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, copyID); err != nil {
				return fmt.Errorf("clearing transfer state for discarded copy %d: %w", copyID, err)
			}
		}
		return nil
	})
}

// ListObsoletePendingMediaAssetTransferCopies は現在の policy から外れた pending 転送コピーを列挙する。
func (s *Media) ListObsoletePendingMediaAssetTransferCopies(ctx context.Context, limit int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	var query transferQueryBuilder
	if _, err := query.appendPlacement(s.policy, s.policy.FSStores()); err != nil {
		return nil, fmt.Errorf("encoding media asset transfer policy: %w", err)
	}
	query.appendSQL(`
SELECT c.id, a.id, c.store, c.rel_path, c.state, a.size_bytes, a.kind, c.generation
FROM media_asset_copies c
JOIN placements a ON a.id = c.media_asset_id
WHERE c.store = ? AND c.state = 'pending' AND (a.kind = 'original' OR ? = FALSE)
  AND `, s.storeName, slices.Contains(s.derivedStagingStores(), s.storeName))
	query.appendPendingCopyObsolete(s.policy.DerivedStore())
	query.appendSQL(`
ORDER BY c.id LIMIT ?`, limit)
	rows, err := queryRows(ctx, s.db, "obsolete pending transfer copies", query.String(), transferCopyCandidate, query.args...)
	if err != nil {
		return nil, fmt.Errorf("listing obsolete pending transfer copies: %w", err)
	}
	return rows, nil
}

// ReclaimObsoletePendingMediaAssetTransferCopy は obsolete の再確認後に owned key を消して pending copy を退役させる。
func (s *Media) ReclaimObsoletePendingMediaAssetTransferCopy(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate, deleteFile func(ownedRelPaths []string) error) (bool, error) {
	if deleteFile == nil {
		return false, errors.New("reclaiming obsolete pending transfer copy: deleter is nil")
	}
	reclaimed := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var recordingID, assetID int64
		err := tx.QueryRowContext(ctx, `SELECT a.recording_id, c.media_asset_id
FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.id = ? AND c.store = ? AND c.state = 'pending'`, candidate.CopyID, s.storeName).Scan(&recordingID, &assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading pending transfer copy %d before reclaim: %w", candidate.CopyID, err)
		}
		if err := lockRecordingState(ctx, tx, recordingID); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return fmt.Errorf("locking recording %d before pending transfer reclaim: %w", recordingID, err)
		}
		var lockedAsset int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE id = ? FOR UPDATE`, assetID).Scan(&lockedAsset); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("locking media asset %d before pending transfer reclaim: %w", assetID, err)
		}
		var lockedCopy int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM media_asset_copies
WHERE id = ? AND store = ? AND state = 'pending' FOR UPDATE`, candidate.CopyID, s.storeName).Scan(&lockedCopy); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("locking pending transfer copy %d: %w", candidate.CopyID, err)
		}
		var checkQuery transferQueryBuilder
		if _, err := checkQuery.appendPlacement(s.policy, s.policy.FSStores()); err != nil {
			return fmt.Errorf("encoding media asset transfer policy: %w", err)
		}
		var obsolete bool
		checkQuery.appendSQL(`SELECT `)
		checkQuery.appendPendingCopyObsolete(s.policy.DerivedStore())
		checkQuery.appendSQL(`
FROM placements a JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE c.id = ? AND c.store = ?`, candidate.CopyID, s.storeName)
		if err := tx.QueryRowContext(ctx, checkQuery.String(), checkQuery.args...).Scan(&obsolete); err != nil {
			return fmt.Errorf("rechecking pending transfer copy %d policy: %w", candidate.CopyID, err)
		}
		if !obsolete {
			return nil
		}
		paths, err := queryRows(ctx, tx, "owned objects for pending transfer copy", `
SELECT object.rel_path FROM media_asset_transfer_objects object
JOIN media_asset_copies c ON c.id = object.copy_id
WHERE object.copy_id = ? AND c.store = ? AND c.state = 'pending'
ORDER BY object.rel_path`, func(rows *sql.Rows) (string, error) {
			var path string
			err := rows.Scan(&path)
			return path, err
		}, candidate.CopyID, s.storeName)
		if err != nil {
			return fmt.Errorf("listing owned objects for pending transfer copy %d: %w", candidate.CopyID, err)
		}
		if paths == nil {
			paths = []string{}
		}
		if err := deleteFile(paths); err != nil {
			return fmt.Errorf("deleting pending transfer copy %d files: %w", candidate.CopyID, err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE media_asset_copies
SET state = 'deleted', deleted_at = CURRENT_TIMESTAMP(6)
WHERE id = ? AND store = ? AND state = 'pending'`, candidate.CopyID, s.storeName)
		if err != nil {
			return fmt.Errorf("marking obsolete pending transfer copy %d deleted: %w", candidate.CopyID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting obsolete pending transfer copies marked deleted: %w", err)
		}
		// ロックしたコピーは pending だったため、deleted への更新は 1 行に効く必要がある。
		if n != 1 {
			return fmt.Errorf("marking obsolete pending transfer copy %d deleted updated %d rows, want one", candidate.CopyID, n)
		}
		for _, statement := range []string{
			`DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`,
			`DELETE FROM media_asset_transfer_attempts WHERE copy_id = ?`,
			`DELETE FROM media_asset_transfer_objects WHERE copy_id = ?`,
			`DELETE FROM media_asset_copy_transfer_progress WHERE copy_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, candidate.CopyID); err != nil {
				return fmt.Errorf("clearing obsolete transfer state for copy %d: %w", candidate.CopyID, err)
			}
		}
		reclaimed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return reclaimed, nil
}

// AcquireMediaAssetTransferProgress は pending copy の進捗 lease を取る。
func (s *Media) AcquireMediaAssetTransferProgress(ctx context.Context, copyID int64, owner uuid.UUID, ttl time.Duration) (store.TransferProgress, bool, error) {
	var progress store.TransferProgress
	acquired := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM media_asset_copies WHERE id = ? AND state = 'pending' FOR UPDATE`, copyID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking pending transfer copy %d: %w", copyID, err)
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		var current struct {
			owner           []byte
			leaseExpiresAt  time.Time
			generation      int64
			handedOff       bool
			offset          int64
			hashState       []byte
			uploadStore     sql.NullString
			uploadState     []byte
			verifyingSource bool
		}
		err = tx.QueryRowContext(ctx, `SELECT lease_owner, lease_expires_at, writer_generation,
handed_off, offset_bytes, hash_state, upload_store, upload_state, verifying_source
FROM media_asset_copy_transfer_progress WHERE copy_id = ? FOR UPDATE`, copyID).
			Scan(&current.owner, &current.leaseExpiresAt, &current.generation, &current.handedOff, &current.offset,
				&current.hashState, &current.uploadStore, &current.uploadState, &current.verifyingSource)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO media_asset_copy_transfer_progress
(copy_id, lease_owner, lease_expires_at, writer_generation, handed_off, offset_bytes, hash_state)
VALUES (?, ?, ?, 1, FALSE, 0, NULL)`, copyID, owner[:], now.Add(ttl)); err != nil {
				return fmt.Errorf("inserting transfer progress for copy %d: %w", copyID, translateError(err))
			}
			progress = store.TransferProgress{WriterGeneration: 1}
			acquired = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking transfer progress for copy %d: %w", copyID, err)
		}
		if !current.handedOff && current.leaseExpiresAt.After(now) {
			return nil
		}
		progress = store.TransferProgress{PrevWriterGeneration: current.generation, WriterGeneration: current.generation + 1}
		if current.handedOff {
			progress = store.TransferProgress{
				PrevWriterGeneration: current.generation, WriterGeneration: current.generation,
				Resumed: true, Offset: current.offset, HashState: current.hashState,
			}
			if current.uploadStore.Valid {
				progress.UploadStore = current.uploadStore.String
				progress.UploadState = current.uploadState
			}
		}
		progress.VerifyingSource = current.verifyingSource
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copy_transfer_progress
SET lease_owner = ?, lease_expires_at = ?, writer_generation = ?, handed_off = FALSE,
    offset_bytes = ?, hash_state = ? WHERE copy_id = ?`, owner[:], now.Add(ttl), progress.WriterGeneration,
			progress.Offset, nullBytes(progress.HashState), copyID); err != nil {
			return fmt.Errorf("claiming transfer progress for copy %d: %w", copyID, err)
		}
		acquired = true
		return nil
	})
	if err != nil {
		return store.TransferProgress{}, false, err
	}
	return progress, acquired, nil
}

func lockLeasedTransferProgress(ctx context.Context, tx *sql.Tx, copyID int64, owner uuid.UUID, generation int64) (time.Time, error) {
	var current struct {
		owner          []byte
		generation     int64
		leaseExpiresAt time.Time
	}
	err := tx.QueryRowContext(ctx, `SELECT lease_owner, writer_generation, lease_expires_at
FROM media_asset_copy_transfer_progress WHERE copy_id = ? FOR UPDATE`, copyID).
		Scan(&current.owner, &current.generation, &current.leaseExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, store.ErrTransferLeaseLost
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("locking transfer progress for copy %d: %w", copyID, err)
	}
	now, err := transactionTime(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	if !bytesEqual(current.owner, owner[:]) || current.generation != generation || !current.leaseExpiresAt.After(now) {
		return time.Time{}, store.ErrTransferLeaseLost
	}
	return now, nil
}

// SaveMediaAssetTransferProgress は slice の終わりの状態を保存し、lease を切る。
func (s *Media) SaveMediaAssetTransferProgress(ctx context.Context, copyID int64, owner uuid.UUID, generation, offset int64, hashState, uploadState []byte) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := lockLeasedTransferProgress(ctx, tx, copyID, owner, generation)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copy_transfer_progress
SET handed_off = TRUE, offset_bytes = ?, hash_state = ?, upload_state = ?, lease_expires_at = ?
WHERE copy_id = ?`, offset, nullBytes(hashState), nullBytes(uploadState), now, copyID); err != nil {
			return fmt.Errorf("saving transfer progress for copy %d: %w", copyID, err)
		}
		return nil
	})
	if errors.Is(err, store.ErrTransferLeaseLost) {
		return store.ErrTransferLeaseLost
	}
	if err != nil {
		return fmt.Errorf("saving transfer progress for copy %d: %w", copyID, err)
	}
	return nil
}

// ReplaceMediaAssetTransferUpload は lease 中の送信状態を置き換え、古い状態を掃除待ちへ移す。
func (s *Media) ReplaceMediaAssetTransferUpload(ctx context.Context, copyID int64, owner uuid.UUID, generation int64, uploadStore string, uploadState []byte) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := lockLeasedTransferProgress(ctx, tx, copyID, owner, generation); err != nil {
			return err
		}
		var current struct {
			store sql.NullString
			state []byte
		}
		if err := tx.QueryRowContext(ctx, `SELECT upload_store, upload_state
FROM media_asset_copy_transfer_progress WHERE copy_id = ?`, copyID).Scan(&current.store, &current.state); err != nil {
			return fmt.Errorf("reading previous transfer upload for copy %d: %w", copyID, err)
		}
		if current.state != nil {
			if !current.store.Valid {
				return fmt.Errorf("transfer progress for copy %d has an upload state without a store", copyID)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO media_asset_transfer_abandoned_uploads (store, upload_state) VALUES (?, ?)`, current.store.String, current.state); err != nil {
				return fmt.Errorf("recording abandoned upload for copy %d: %w", copyID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copy_transfer_progress
SET upload_store = ?, upload_state = ?, offset_bytes = 0, hash_state = NULL, handed_off = FALSE
WHERE copy_id = ?`, uploadStore, nullBytes(uploadState), copyID); err != nil {
			return fmt.Errorf("replacing transfer upload for copy %d: %w", copyID, err)
		}
		return nil
	})
	if errors.Is(err, store.ErrTransferLeaseLost) {
		return store.ErrTransferLeaseLost
	}
	if err != nil {
		return fmt.Errorf("replacing transfer upload for copy %d: %w", copyID, err)
	}
	return nil
}

// BeginMediaAssetTransferSourceVerification は lease 中の原本照合を始める。
func (s *Media) BeginMediaAssetTransferSourceVerification(ctx context.Context, copyID int64, owner uuid.UUID, generation int64) error {
	return s.updateTransferVerification(ctx, copyID, owner, generation, true)
}

// EndMediaAssetTransferSourceVerification は lease 中の原本照合を終える。
func (s *Media) EndMediaAssetTransferSourceVerification(ctx context.Context, copyID int64, owner uuid.UUID, generation int64) error {
	return s.updateTransferVerification(ctx, copyID, owner, generation, false)
}

func (s *Media) updateTransferVerification(ctx context.Context, copyID int64, owner uuid.UUID, generation int64, verifying bool) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := lockLeasedTransferProgress(ctx, tx, copyID, owner, generation); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copy_transfer_progress
SET verifying_source = ?, offset_bytes = 0, hash_state = NULL WHERE copy_id = ?`, verifying, copyID); err != nil {
			return fmt.Errorf("updating source verification for transfer copy %d: %w", copyID, err)
		}
		return nil
	})
	if errors.Is(err, store.ErrTransferLeaseLost) {
		return store.ErrTransferLeaseLost
	}
	if err != nil {
		return err
	}
	return nil
}

// RecordObservedMediaAssetSHA256 は世代が現行で SHA-256 が未知の資産だけを更新する。
func (s *Media) RecordObservedMediaAssetSHA256(ctx context.Context, mediaAssetID, generation int64, sha256 []byte) (bool, error) {
	recorded := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var currentGeneration int64
		var existing []byte
		if err := tx.QueryRowContext(ctx, `SELECT derived_generation, sha256 FROM media_assets WHERE id = ? FOR UPDATE`, mediaAssetID).
			Scan(&currentGeneration, &existing); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("locking media asset %d before recording its digest: %w", mediaAssetID, err)
		}
		if currentGeneration != generation || existing != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_assets SET sha256 = ? WHERE id = ?`, sha256, mediaAssetID); err != nil {
			return fmt.Errorf("recording observed SHA-256 for media asset %d: %w", mediaAssetID, err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("recording observed SHA-256 for media asset %d: %w", mediaAssetID, err)
	}
	return recorded, nil
}

// ListAbandonedTransferUploads は置き場 store の掃除待ちを id 順に返す。
func (s *Media) ListAbandonedTransferUploads(ctx context.Context, storeName string, limit int32) ([]store.AbandonedTransferUpload, error) {
	rows, err := queryRows(ctx, s.db, "abandoned transfer uploads", `
SELECT id, upload_state FROM media_asset_transfer_abandoned_uploads
WHERE store = ? ORDER BY id LIMIT ?`, func(rows *sql.Rows) (store.AbandonedTransferUpload, error) {
		var out store.AbandonedTransferUpload
		err := rows.Scan(&out.ID, &out.State)
		return out, err
	}, storeName, limit)
	if err != nil {
		return nil, fmt.Errorf("listing abandoned uploads for store %q: %w", storeName, err)
	}
	return rows, nil
}

// DeleteAbandonedTransferUpload は掃除済みの送信状態を削除する。
func (s *Media) DeleteAbandonedTransferUpload(ctx context.Context, id int64) error {
	if err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_asset_transfer_abandoned_uploads WHERE id = ?`, id); err != nil {
			return fmt.Errorf("deleting abandoned upload %d: %w", id, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// ReleaseMediaAssetTransferProgress は owner の lease を切り、次の取得で世代を上げる。
func (s *Media) ReleaseMediaAssetTransferProgress(ctx context.Context, copyID int64, owner uuid.UUID) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var current []byte
		if err := tx.QueryRowContext(ctx, `SELECT lease_owner FROM media_asset_copy_transfer_progress WHERE copy_id = ? FOR UPDATE`, copyID).Scan(&current); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("locking transfer progress for copy %d: %w", copyID, err)
		}
		if !bytesEqual(current, owner[:]) {
			return nil
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copy_transfer_progress
SET lease_expires_at = ?, handed_off = FALSE WHERE copy_id = ?`, now, copyID); err != nil {
			return fmt.Errorf("releasing transfer progress for copy %d: %w", copyID, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("releasing transfer progress for copy %d: %w", copyID, err)
	}
	return nil
}

// ListMediaAssetCopiesToDelete は overlap を過ぎ、current policy で置き場から外れたコピーを返す。
func (s *Media) ListMediaAssetCopiesToDelete(ctx context.Context, overlapCutoff time.Time, limit int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	policy := s.policy.PlacementPolicy()
	rules, err := originalRulesJSON(policy)
	if err != nil {
		return nil, fmt.Errorf("encoding media placement policy: %w", err)
	}
	query := `
SELECT c.id, c.media_asset_id, c.store, c.rel_path, c.state,
       a.size_bytes, a.kind, c.generation
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.store = ? AND c.state = 'active'
  AND ` + prunableCopySQL + `
ORDER BY c.id LIMIT ?`
	args := []any{s.storeName, rules, policy.DerivedStore(), overlapCutoff, limit}
	rows, err := queryRows(ctx, s.db, "stale media asset copies", query, transferCopyCandidate, args...)
	if err != nil {
		return nil, fmt.Errorf("listing stale media asset copies: %w", err)
	}
	return rows, nil
}

// GetMediaAssetCopyForDeletion はジョブ引数の copy ID から現在のコピー状態を返す。
func (s *Media) GetMediaAssetCopyForDeletion(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate) (store.MediaAssetCopyDeleteCandidate, bool, error) {
	var out store.MediaAssetCopyDeleteCandidate
	err := s.db.QueryRowContext(ctx, `
SELECT c.id, c.media_asset_id, c.store, c.rel_path, c.state, a.size_bytes, a.kind, c.generation
FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.id = ? AND c.media_asset_id = ? AND c.store = ?`, candidate.CopyID, candidate.MediaAssetID, candidate.Store).
		Scan(&out.CopyID, &out.MediaAssetID, &out.Store, &out.RelPath, &out.State, &out.SizeBytes, &out.Kind, &out.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MediaAssetCopyDeleteCandidate{}, false, nil
	}
	if err != nil {
		return store.MediaAssetCopyDeleteCandidate{}, false, fmt.Errorf("getting media asset %d copy in %q for deletion: %w", candidate.MediaAssetID, candidate.Store, err)
	}
	return out, true, nil
}

// MarkMediaAssetCopyDeleting は asset 行をロックしてから削除条件を再評価し、copy を deleting にする。
func (s *Media) MarkMediaAssetCopyDeleting(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate, overlapCutoff time.Time) (string, bool, error) {
	policy := s.policy.PlacementPolicy()
	rules, err := originalRulesJSON(policy)
	if err != nil {
		return "", false, fmt.Errorf("encoding media placement policy: %w", err)
	}
	var relPath string
	marked := false
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		var assetID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE id = ? FOR UPDATE`, candidate.MediaAssetID).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking logical media asset %d before copy deletion: %w", candidate.MediaAssetID, err)
		}
		var currentPath string
		err = tx.QueryRowContext(ctx, `SELECT rel_path FROM media_asset_copies
WHERE id = ? AND media_asset_id = ? AND store = ? AND state = 'active' FOR UPDATE`,
			candidate.CopyID, candidate.MediaAssetID, candidate.Store).Scan(&currentPath)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking media asset copy %d before deletion: %w", candidate.CopyID, err)
		}
		eligibleQuery := `SELECT EXISTS (
SELECT 1 FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.id = ? AND c.media_asset_id = ? AND c.store = ? AND c.state = 'active'
  AND ` + prunableCopySQL + `)`
		var eligible bool
		if err := tx.QueryRowContext(ctx, eligibleQuery, candidate.CopyID, candidate.MediaAssetID, candidate.Store,
			rules, policy.DerivedStore(), overlapCutoff).Scan(&eligible); err != nil {
			return fmt.Errorf("rechecking media asset copy %d placement before deletion: %w", candidate.CopyID, err)
		}
		if !eligible {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copies SET state = 'deleting', deleted_at = NULL
WHERE id = ? AND media_asset_id = ? AND store = ? AND state = 'active'`, candidate.CopyID, candidate.MediaAssetID, candidate.Store); err != nil {
			return fmt.Errorf("marking media asset %d copy in %q deleting: %w", candidate.MediaAssetID, candidate.Store, err)
		}
		relPath = currentPath
		marked = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return relPath, marked, nil
}

// MarkMediaAssetCopyDeleted は unlink 済みの deleting copy を deleted にする。
func (s *Media) MarkMediaAssetCopyDeleted(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate) (bool, error) {
	current, found, err := s.GetMediaAssetCopyForDeletion(ctx, candidate)
	if err != nil || !found || current.State != "deleting" {
		return false, err
	}
	if err := s.markCopyDeleted(ctx, candidate.CopyID, candidate.Store); err != nil {
		return false, err
	}
	return true, nil
}

const transferBacklogLockName = "transfer_backlog_snapshots"

// lockTransferBacklog はバックログ snapshot の更新を直列化する fixed lock row を取る。
func lockTransferBacklog(ctx context.Context, tx *sql.Tx) error {
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM named_locks WHERE name = ? FOR UPDATE`, transferBacklogLockName).Scan(&name); err != nil {
		return fmt.Errorf("locking transfer backlog snapshots: %w", err)
	}
	return nil
}

// ReadTransferBacklog は置き場ごとの直近の転送待ち件数と時刻を返す。
func (s *Media) ReadTransferBacklog(ctx context.Context) (map[string]store.TransferBacklogSnapshot, error) {
	rows, err := queryRows(ctx, s.db, "transfer backlog snapshots", `
SELECT store, assets, counted_at FROM transfer_backlog_snapshots ORDER BY store`, func(rows *sql.Rows) (struct {
		store    string
		snapshot store.TransferBacklogSnapshot
	}, error) {
		var row struct {
			store    string
			snapshot store.TransferBacklogSnapshot
		}
		err := rows.Scan(&row.store, &row.snapshot.Assets, &row.snapshot.CountedAt)
		return row, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading transfer backlog snapshots: %w", err)
	}
	out := make(map[string]store.TransferBacklogSnapshot, len(rows))
	for _, row := range rows {
		out[row.store] = row.snapshot
	}
	return out, nil
}

// RecordTransferBacklog は直列化された tx で、より新しい countedAt の行だけを保存する。
func (s *Media) RecordTransferBacklog(ctx context.Context, counts map[string]int64, countedAt time.Time) (map[string]store.TransferBacklogWrite, error) {
	// DATETIME(6) は MySQL で小数秒を丸めるため、比較と保存の両方を同じ精度に揃える。
	countedAt = countedAt.Round(time.Microsecond)
	written := make(map[string]store.TransferBacklogWrite, len(counts))
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockTransferBacklog(ctx, tx); err != nil {
			return err
		}
		type snapshot struct {
			assets int64
			at     time.Time
		}
		storedRows, err := queryRows(ctx, tx, "locked transfer backlog snapshots", `
SELECT store, assets, counted_at FROM transfer_backlog_snapshots ORDER BY store FOR UPDATE`, func(rows *sql.Rows) (struct {
			store string
			value snapshot
		}, error) {
			var row struct {
				store string
				value snapshot
			}
			err := rows.Scan(&row.store, &row.value.assets, &row.value.at)
			return row, err
		})
		if err != nil {
			return fmt.Errorf("reading previous transfer backlog snapshots: %w", err)
		}
		previous := make(map[string]snapshot, len(storedRows))
		for _, row := range storedRows {
			previous[row.store] = row.value
		}
		stores := make([]string, 0, len(counts))
		for name := range counts {
			stores = append(stores, name)
		}
		sort.Strings(stores)
		if len(stores) == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM transfer_backlog_snapshots`); err != nil {
				return fmt.Errorf("deleting transfer backlog snapshots without a limit: %w", err)
			}
		} else {
			condition, args := notInSQL("store", stores)
			if _, err := tx.ExecContext(ctx, `DELETE FROM transfer_backlog_snapshots WHERE `+condition, args...); err != nil {
				return fmt.Errorf("deleting transfer backlog snapshots without a limit: %w", err)
			}
		}
		for _, name := range stores {
			before, had := previous[name]
			if had && !countedAt.After(before.at) {
				written[name] = store.TransferBacklogWrite{Current: before.assets}
				continue
			}
			if had {
				if _, err := tx.ExecContext(ctx, `UPDATE transfer_backlog_snapshots SET assets = ?, counted_at = ? WHERE store = ?`, counts[name], countedAt, name); err != nil {
					return fmt.Errorf("updating transfer backlog snapshot for store %q: %w", name, err)
				}
			} else if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_backlog_snapshots (store, assets, counted_at) VALUES (?, ?, ?)`, name, counts[name], countedAt); err != nil {
				return fmt.Errorf("inserting transfer backlog snapshot for store %q: %w", name, err)
			}
			written[name] = store.TransferBacklogWrite{Current: counts[name], Applied: true, HadPrevious: had, Previous: before.assets}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// DatabaseClock は DB の現在時刻を返す。
func (s *Media) DatabaseClock(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP(6)`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("reading database clock: %w", err)
	}
	return now, nil
}
