package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.MediaServing    = (*Media)(nil)
	_ store.OriginalLocator = (*Media)(nil)
)

// Media は録画のメディア（原本・encoded・サムネイル・シークタイル）の配信・reconcile・公開を MySQL で実装する。
// 束縛する置き場（storeName・originalStore・方針・配信の読める置き場）は pgstore.Media と同じ意味を持つ。
type Media struct {
	db            *sql.DB
	storeName     string
	originalStore string
	policy        store.StoragePolicy
	// servingStores が非 nil なら、派生物の配信で読める置き場をこの一覧に絞る。
	servingStores []string
}

// NewMediaUnboundWithPolicy は置き場で絞らず、方針で判定する Media を作る。
// 置き場をまたぐ reconcile・転送のワーカーが使う。
func NewMediaUnboundWithPolicy(db *sql.DB, policy store.StoragePolicy) *Media {
	return &Media{db: db, policy: policy}
}

// NewMediaInStore は storeName の置き場に束縛し、原本も同じ置き場から読む。
func NewMediaInStore(db *sql.DB, storeName string, policy store.StoragePolicy) *Media {
	return &Media{db: db, storeName: storeName, originalStore: storeName, policy: policy}
}

// NewMediaForDerivedStore は派生物を derivedStore に書き、原本は originalStore から読む Media を作る。
// 置き場が二つ要るのは、派生物の公開だけが原本の置き場と派生物の置き場を分けるから。
func NewMediaForDerivedStore(db *sql.DB, derivedStore, originalStore string, policy store.StoragePolicy) *Media {
	return &Media{db: db, storeName: derivedStore, originalStore: originalStore, policy: policy}
}

// NewMediaForStreamer は配置方針から derived store を選び、読める置き場だけを配信候補にする Media を作る。
// streamer は置き場に書かず読むだけなので、束縛する置き場を持たない。
func NewMediaForStreamer(db *sql.DB, policy store.StoragePolicy, readableStores []string) *Media {
	stores := make([]string, len(readableStores))
	copy(stores, readableStores)
	return &Media{db: db, policy: policy, servingStores: stores}
}

// RequireAdoptedStoreRows は割り当て前の行（範囲が空の delete_reconcile ブレーカーを含む）や
// コピー欠落が残っていないことを確認する。PostgreSQL の pgstore と同じ判定（CountUnassignedStorageRows）。
func (s *Media) RequireAdoptedStoreRows(ctx context.Context) error {
	var count int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM media_assets a WHERE NOT EXISTS (
			SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
		)) + (SELECT COUNT(*) FROM circuit_breakers WHERE scope = '' AND name = 'delete_reconcile')
	`).Scan(&count); err != nil {
		return fmt.Errorf("checking for unassigned media store rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("%d media store row(s) have no store or media asset copy (including delete_reconcile circuit breaker rows with an empty scope); run `rokuban storage adopt <store-name>` before starting workers", count)
	}
	return nil
}

// derivedStoreName は派生物の書き込み先の置き場を返す。方針に無ければ束縛された置き場。
func (s *Media) derivedStoreName() string {
	if s.policy.DerivedStore() != "" {
		return s.policy.DerivedStore()
	}
	return s.storeName
}

// derivedStagingStores は派生物を一時的に置く FS 置き場の一覧を返す。pgstore の同名の関数と同じ判定。
func (s *Media) derivedStagingStores() []string {
	fsStores := s.policy.FSStores()
	desired := s.policy.DerivedStore()
	if desired == "" || slices.Contains(fsStores, desired) {
		return nil
	}
	return fsStores
}

// servingSelection は派生物の配信で選ぶコピーの置き場（優先する置き場と読める置き場）を返す。
// readable が nil なら全置き場が読める扱い。
func (s *Media) servingSelection() (preferred string, readable []string) {
	if s.servingStores != nil {
		return s.policy.DerivedStore(), s.servingStores
	}
	name := s.derivedStoreName()
	if name == "" {
		return "", nil
	}
	return name, []string{name}
}

// preferredActiveCopyLateral は media_assets a の現行の active コピーを 1 つ選ぶ。
// Postgres の preferred_active_media_asset_copy に対応する。置き場は ? で束縛し、NULL なら絞らない。
// 置き場名順で先頭を選ぶ。読み手は a を自分の media_assets の別名として使い、c を結果に使う。
const preferredActiveCopyLateral = `JOIN LATERAL (
    SELECT c.rel_path, c.store
    FROM media_asset_copies c
    WHERE c.media_asset_id = a.id AND c.state = 'active' AND c.generation = a.derived_generation
      AND (? IS NULL OR c.store = ?)
    ORDER BY c.store
    LIMIT 1
) c ON TRUE`

// preferredReadableCopyLateral は配信の現行コピーを選ぶ。readable が nil なら全置き場、空なら何も読めない。
// 優先する置き場を先に、残りは置き場名順。Postgres の preferred_readable_media_asset_copy に対応する。
// 引数は preferred, preferred, readable... の順（readable が絞らないときは readable の引数は無い）。
func preferredReadableCopyLateral(readable []string) string {
	filter := "TRUE"
	if readable != nil {
		if len(readable) == 0 {
			filter = "FALSE"
		} else {
			filter = "c.store IN (" + qmarks(len(readable)) + ")"
		}
	}
	return `JOIN LATERAL (
    SELECT c.rel_path, c.store
    FROM media_asset_copies c
    WHERE c.media_asset_id = a.id AND c.state = 'active' AND c.generation = a.derived_generation
      AND ` + filter + `
    ORDER BY (c.store = ?) DESC, c.store
    LIMIT 1
) c ON TRUE`
}

// placeholders は n 個の ? を、カンマで区切って返す。n が 0 なら空文字。
func qmarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// anyArgs は文字列の並びを SQL の引数に写す。
func anyArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// nullableString は空文字を NULL の引数にする（preferred_active の「置き場を限定しない」）。
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// mediaQueryError は読み取りの失敗を、行が無いときは store.ErrNotFound に写して文脈を付ける。
func mediaQueryError(err error, context string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", context, store.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// servingRow は配信の問い合わせが返す原本・派生物の 1 行。
type servingRow struct {
	ID        int64
	RelPath   string
	SizeBytes int64
	Store     string
}

func queryServingAsset(ctx context.Context, db *sql.DB, desc, query string, args ...any) (store.MediaAsset, error) {
	var row servingRow
	err := db.QueryRowContext(ctx, query, args...).Scan(&row.ID, &row.RelPath, &row.SizeBytes, &row.Store)
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, desc)
	}
	return store.MediaAsset{ID: row.ID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetOriginalMediaAssetForServing は配信可能な原本を返す。ごみ箱の録画は配らない。
func (s *Media) GetOriginalMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	return s.getOriginalForServing(ctx, recordingID, nullableString(s.originalStore))
}

// GetOriginalMediaAssetForServingInStore は storeName の置き場にコピーがある配信可能な原本を返す。
func (s *Media) GetOriginalMediaAssetForServingInStore(ctx context.Context, recordingID int64, storeName string) (store.MediaAsset, error) {
	return s.getOriginalForServing(ctx, recordingID, storeName)
}

func (s *Media) getOriginalForServing(ctx context.Context, recordingID int64, storeName any) (store.MediaAsset, error) {
	return queryServingAsset(ctx, s.db, fmt.Sprintf("loading original media asset for recording %d", recordingID), `
SELECT a.id, c.rel_path, a.size_bytes, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
`+preferredActiveCopyLateral+`
WHERE a.recording_id = ?
  AND a.kind = 'original'
  AND r.deleted_at IS NULL`,
		storeName, storeName, recordingID)
}

// GetEncodedMediaAssetForServing は指定プロファイルの配信可能な encoded を返す。
func (s *Media) GetEncodedMediaAssetForServing(ctx context.Context, recordingID int64, profile string) (store.MediaAsset, error) {
	preferred, readable := s.servingSelection()
	return queryServingAsset(ctx, s.db, fmt.Sprintf("loading encoded media asset for recording %d profile %q", recordingID, profile), `
SELECT a.id, c.rel_path, a.size_bytes, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
`+preferredReadableCopyLateral(readable)+`
WHERE a.recording_id = ?
  AND a.kind = 'encoded'
  AND a.profile = ?
  AND r.deleted_at IS NULL`, s.readableCall(readable, preferred, recordingID, profile)...)
}

// GetThumbnailMediaAssetForServing は配信可能なサムネイルを返す。
func (s *Media) GetThumbnailMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	return s.getDerivedForServing(ctx, recordingID, "thumbnail")
}

// GetSeekTilesMediaAssetForServing は配信可能なシークタイルを返す。
func (s *Media) GetSeekTilesMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	return s.getDerivedForServing(ctx, recordingID, "seek_tiles")
}

func (s *Media) getDerivedForServing(ctx context.Context, recordingID int64, kind string) (store.MediaAsset, error) {
	preferred, readable := s.servingSelection()
	return queryServingAsset(ctx, s.db, fmt.Sprintf("loading %s media asset for recording %d", kind, recordingID),
		`SELECT a.id, c.rel_path, a.size_bytes, c.store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
`+preferredReadableCopyLateral(readable)+`
WHERE a.recording_id = ?
  AND a.kind = ?
  AND r.deleted_at IS NULL`, s.readableCall(readable, preferred, recordingID, kind)...)
}

// readableCall は preferredReadableCopyLateral の引数を、SQL の出現順に並べる。
// 読める置き場が絞られるなら IN の引数が先、その後に優先置き場、最後に録画 ID などの WHERE の引数。
func (s *Media) readableCall(readable []string, preferred string, tail ...any) []any {
	args := make([]any, 0, len(readable)+len(tail)+1)
	if readable != nil {
		args = append(args, anyArgs(readable)...)
	}
	args = append(args, nullableString(preferred))
	return append(args, tail...)
}

// GetActiveOriginalMediaAsset は録画 HLS 配信に使う active な原本を返す。
func (s *Media) GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	return queryServingAsset(ctx, s.db, fmt.Sprintf("loading active original media asset for recording %d", recordingID), `
SELECT a.id, c.rel_path, a.size_bytes, c.store
FROM media_assets a
`+preferredActiveCopyLateral+`
WHERE a.recording_id = ?
  AND a.kind = 'original'`,
		nullableString(s.originalStore), nullableString(s.originalStore), recordingID)
}

// OriginalStore は録画の active な原本のコピーがある置き場を、この Media の束縛によらず返す。
func (s *Media) OriginalStore(ctx context.Context, recordingID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `
SELECT c.store
FROM media_assets a
`+preferredActiveCopyLateral+`
WHERE a.recording_id = ? AND a.kind = 'original'`, nil, nil, recordingID).Scan(&name)
	if err != nil {
		return "", mediaQueryError(err, fmt.Sprintf("locating the active original of recording %d", recordingID))
	}
	return name, nil
}

// OriginalStores は recordingIDs の active な原本のコピーの置き場を 1 回の問い合わせで返す。
func (s *Media) OriginalStores(ctx context.Context, recordingIDs []int64) (map[int64]string, error) {
	stores := make(map[int64]string, len(recordingIDs))
	if len(recordingIDs) == 0 {
		return stores, nil
	}
	args := []any{nil, nil}
	for _, id := range recordingIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT a.recording_id, c.store
FROM media_assets a
`+preferredActiveCopyLateral+`
WHERE a.kind = 'original'
  AND a.recording_id IN (`+qmarks(len(recordingIDs))+`)
ORDER BY a.recording_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("locating the active originals of %d recordings: %w", len(recordingIDs), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scanning active original store: %w", err)
		}
		stores[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active original stores: %w", err)
	}
	return stores, nil
}

// GetRecordingHLSTarget は配信可能な原本か ingest temp と世代を返す。
// 原本の行と有効なコピーがあれば、status に関係なく原本を返す（commit は mirakc の record の状態で決まる）。
func (s *Media) GetRecordingHLSTarget(ctx context.Context, recordingID int64) (store.RecordingHLSInputTarget, error) {
	var startedAt sql.NullTime
	var originalID, originalSize, originalGeneration sql.NullInt64
	var originalRelPath sql.NullString
	var tempRelPath, tempStore sql.NullString
	var tempSize, tempGeneration sql.NullInt64
	var hasOriginal bool
	err := s.db.QueryRowContext(ctx, `
SELECT r.started_at,
       a.id, c.rel_path, a.size_bytes, COALESCE(a.ingest_generation, 0),
       p.temp_rel_path, p.temp_store, p.written_bytes, p.temp_generation,
       EXISTS (SELECT 1 FROM media_assets any_original
               WHERE any_original.recording_id = r.id AND any_original.kind = 'original')
FROM recordings r
LEFT JOIN media_assets a ON a.recording_id = r.id AND a.kind = 'original'
    AND EXISTS (SELECT 1 FROM active_media_asset_copies ac WHERE ac.media_asset_id = a.id AND ac.store = ?)
LEFT JOIN LATERAL (
    SELECT copy.rel_path
    FROM media_asset_copies copy
    WHERE copy.media_asset_id = a.id AND copy.state = 'active' AND copy.store = ?
      AND copy.generation = a.derived_generation
    ORDER BY copy.store
    LIMIT 1
) c ON TRUE
LEFT JOIN recording_ingest_progress p ON p.recording_id = r.id
WHERE r.id = ? AND r.deleted_at IS NULL AND r.purged_at IS NULL AND r.superseded_at IS NULL
  AND (a.id IS NOT NULL OR (p.recording_id IS NOT NULL AND NOT EXISTS (
      SELECT 1 FROM media_assets any_original
      WHERE any_original.recording_id = r.id AND any_original.kind = 'original')))`,
		s.storeName, s.storeName, recordingID).Scan(
		&startedAt,
		&originalID, &originalRelPath, &originalSize, &originalGeneration,
		&tempRelPath, &tempStore, &tempSize, &tempGeneration,
		&hasOriginal,
	)
	if err != nil {
		return store.RecordingHLSInputTarget{}, mediaQueryError(err, fmt.Sprintf("loading recording HLS target for recording %d", recordingID))
	}
	var started *time.Time
	if startedAt.Valid {
		started = &startedAt.Time
	}
	if originalID.Valid {
		return store.RecordingHLSInputTarget{
			Kind:             store.RecordingHLSInputOriginal,
			ID:               originalID.Int64,
			RelPath:          originalRelPath.String,
			SizeBytes:        originalSize.Int64,
			IngestGeneration: originalGeneration.Int64,
			StartedAt:        started,
		}, nil
	}
	if hasOriginal || !tempRelPath.Valid || !tempStore.Valid || !tempGeneration.Valid {
		return store.RecordingHLSInputTarget{}, fmt.Errorf("recording %d has no readable HLS input: %w", recordingID, store.ErrNotFound)
	}
	return store.RecordingHLSInputTarget{
		Kind:             store.RecordingHLSInputTemp,
		RelPath:          tempRelPath.String,
		StoreName:        tempStore.String,
		SizeBytes:        tempSize.Int64,
		IngestGeneration: tempGeneration.Int64,
		StartedAt:        started,
	}, nil
}

// GetRecordingRemovalState は録画の削除・purge・supersede の状態を返す。
func (s *Media) GetRecordingRemovalState(ctx context.Context, recordingID int64) (store.RecordingRemovalState, error) {
	var deletedAt, purgedAt, supersededAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT deleted_at, purged_at, superseded_at FROM recordings WHERE id = ?`, recordingID).
		Scan(&deletedAt, &purgedAt, &supersededAt)
	if err != nil {
		return store.RecordingRemovalState{}, mediaQueryError(err, fmt.Sprintf("loading recording removal state for recording %d", recordingID))
	}
	return store.RecordingRemovalState{
		DeletedAt:    nullTimePtr(deletedAt),
		PurgedAt:     nullTimePtr(purgedAt),
		SupersededAt: nullTimePtr(supersededAt),
	}, nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
