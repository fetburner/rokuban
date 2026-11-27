package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.MediaDelete = (*Media)(nil)

// 以下の断片は、Postgres の削除述語（internal/db/schema.sql）の MySQL 版である。
// 述語は 2 つ以上の読み手（列挙・遷移・戻し）が使うので、方言ごとに 1 か所だけ定義する。
// 断片の a は media_assets、c は media_asset_copies の別名。? の順は断片ごとに決まっていて、
// 呼び出し側の引数の並びがそれに従う。

// untilEncodedDeletableSQL は until_encoded_deletable_originals の述語である（a は原本の media_assets）。
// ? は desired の置き場で、4 回現れる（untilEncodedArgs が 4 つ揃える）。desired が nil なら何も満たさない。
const untilEncodedDeletableSQL = `EXISTS (
  SELECT 1
  FROM recordings r
  JOIN recording_processing_policy p ON p.recording_id = r.id
  WHERE r.id = a.recording_id
    AND ? IS NOT NULL
    AND a.kind = 'original'
    AND p.keep_original = 'until_encoded'
    AND r.deleted_at IS NULL
    AND JSON_LENGTH(p.encode_profiles) > 0
    AND NOT EXISTS (SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = r.id)
    AND EXISTS (SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id)
    AND NOT EXISTS (SELECT 1 FROM unencoded_desired_profiles u WHERE u.recording_id = r.id)
    AND NOT EXISTS (
      SELECT 1
      FROM JSON_TABLE(p.encode_profiles, '$[*]' COLUMNS (
          profile VARCHAR(255) COLLATE utf8mb4_0900_bin PATH '$'
      )) want
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          JOIN active_present_media_asset_copies ec ON ec.media_asset_id = e.id AND ec.store = ?
          WHERE e.recording_id = r.id AND e.kind = 'encoded' AND e.profile = want.profile
      )
    )
    AND EXISTS (
      SELECT 1
      FROM media_assets th
      JOIN active_present_media_asset_copies tc ON tc.media_asset_id = th.id AND tc.store = ?
      WHERE th.recording_id = r.id AND th.kind = 'thumbnail'
    )
    AND EXISTS (
      SELECT 1
      FROM media_assets st
      JOIN active_present_media_asset_copies sc ON sc.media_asset_id = st.id AND sc.store = ?
      WHERE st.recording_id = r.id AND st.kind = 'seek_tiles'
    )
    AND (NOT p.cm_detect
         OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
         OR EXISTS (
             SELECT 1 FROM current_cm_attempts ca
             WHERE ca.recording_id = r.id AND ca.state = 'failed' AND NOT (ca.stage <=> 'adopt')
         ))
)`

// trashDeletableAssetSQL は、資産の録画が trash_deletable_recordings に入るかの述語である。? は猶予の cutoff。
const trashDeletableAssetSQL = `EXISTS (SELECT 1 FROM recordings r WHERE r.id = a.recording_id AND ` + trashDeletableRecordingSQL + `)`

// removedEncodedAssetSQL は removed_encoded_assets の述語である（view を引くだけで、引数は無い）。
const removedEncodedAssetSQL = `EXISTS (SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id)`

// oldGenerationCopySQL は old_generation_media_asset_copies の述語である。? は overlap の cutoff。
const oldGenerationCopySQL = `EXISTS (
  SELECT 1 FROM old_generation_media_asset_copies o
  WHERE o.id = c.id AND o.generation_started_at <= ?
)`

// prunableCopySQL は media_asset_copy_prunable の述語である。
// desired は a の種別と方針から決まる置き場（原本は規則を先勝ちで評価し、派生物は派生の置き場）。
// 規則の site と older_than_us（放送終了が now() より古い）で判定する。
// 引数は rules（JSON 配列の文字列）、derived、overlap の順。
var prunableCopySQL = `EXISTS (
  SELECT 1
  FROM recordings r
  JOIN LATERAL (
    SELECT ` + desiredMediaStoreSQL("a.kind", "r.site", "TIMESTAMPADD(MICROSECOND, r.program_duration_ms * 1000, r.program_start_at)") + ` AS store
  ) d ON TRUE
  WHERE r.id = a.recording_id
    AND r.deleted_at IS NULL
    AND c.state IN ('active', 'deleting')
    AND c.generation = a.derived_generation
    AND d.store IS NOT NULL
    AND d.store <> c.store
    AND EXISTS (
      SELECT 1
      FROM active_present_media_asset_copies dc
      JOIN media_asset_copies dcc ON dcc.id = dc.id
      WHERE dc.media_asset_id = a.id AND dc.store = d.store AND dcc.created_at <= ?
    )
)`

// deletionArm は削除条件の 1 つの腕（述語と、その ? の引数）である。
type deletionArm struct {
	sql  string
	args []any
}

// deletionArms は削除条件の 5 つの腕を、pgstore の削除述語と同じ順で返す。
// 配置の腕は PlacementPolicy で判定する（配置削除が無効な方針では派生物も原本も動かない。pgstore と同じ）。
func (s *Media) deletionArms(graceCutoff, overlapCutoff time.Time) ([]deletionArm, error) {
	placement := s.policy.PlacementPolicy()
	rules, err := originalRulesJSON(placement)
	if err != nil {
		return nil, fmt.Errorf("encoding media placement policy: %w", err)
	}
	var derived any
	if d := placement.DerivedStore(); d != "" {
		derived = d
	}
	desired := s.desiredStore()
	return []deletionArm{
		{trashDeletableAssetSQL, []any{graceCutoff}},
		{untilEncodedDeletableSQL, untilEncodedArgs(desired)},
		{removedEncodedAssetSQL, nil},
		{oldGenerationCopySQL, []any{overlapCutoff}},
		{prunableCopySQL, []any{string(rules), derived, overlapCutoff}},
	}, nil
}

// joinArms は腕を OR で結び、引数を連結する。
func joinArms(arms []deletionArm) (string, []any) {
	parts := make([]string, 0, len(arms))
	var args []any
	for _, arm := range arms {
		parts = append(parts, arm.sql)
		args = append(args, arm.args...)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// untilEncodedArgs は untilEncodedDeletableSQL の 4 つの ? に desired を渡す引数を返す。
func untilEncodedArgs(desired any) []any {
	return []any{desired, desired, desired, desired}
}

// desiredStore は派生物の desired な置き場を返す。方針に無ければ nil（述語が何も満たさない）。
func (s *Media) desiredStore() any {
	if d := s.policy.DerivedStore(); d != "" {
		return d
	}
	return nil
}

func scanMediaDeleteAsset(rows *sql.Rows) (store.MediaDeleteAsset, error) {
	var a store.MediaDeleteAsset
	err := rows.Scan(&a.ID, &a.MediaAssetID, &a.RecordingID, &a.RelPath, &a.SizeBytes, &a.Kind, &a.Generation)
	return a, err
}

// listMediaDeleteAssets は storeName の置き場にあり state のコピーのうち、where の述語を満たすものを列挙する。
// where は a と c の述語で、args は where の ? の順に並べる。
func (s *Media) listMediaDeleteAssets(ctx context.Context, desc, state, where string, args []any, limit int32) ([]store.MediaDeleteAsset, error) {
	all := append([]any{s.storeName, state}, args...)
	all = append(all, limit)
	rows, err := queryRows(ctx, s.db, desc, `
SELECT c.id, a.id, a.recording_id, c.rel_path, a.size_bytes, a.kind, c.generation
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id AND c.store = ? AND c.state = ?
WHERE `+where+`
ORDER BY a.id
LIMIT ?`, scanMediaDeleteAsset, all...)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", desc, err)
	}
	return rows, nil
}

// ListPendingDeleteAssets は削除条件を満たす deleting コピーを返す。
func (s *Media) ListPendingDeleteAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	arms, err := s.deletionArms(graceCutoff, overlapCutoff)
	if err != nil {
		return nil, err
	}
	where, args := joinArms(arms)
	return s.listMediaDeleteAssets(ctx, "pending media asset deletes", "deleting", where, args, limit)
}

// ListUnqualifiedDeletingAssets は現在どの削除条件も満たさない deleting コピーを返す。
func (s *Media) ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, limit int32) ([]store.MediaDeleteRevertCandidate, error) {
	arms, err := s.deletionArms(graceCutoff, overlapCutoff)
	if err != nil {
		return nil, err
	}
	where, args := joinArms(arms)
	all := append([]any{s.storeName}, args...)
	all = append(all, limit)
	rows, err := queryRows(ctx, s.db, "unqualified deleting media asset copies", `
SELECT c.id, a.id, a.recording_id, c.rel_path, a.kind
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id AND c.store = ? AND c.state = 'deleting'
WHERE NOT `+where+`
ORDER BY a.id
LIMIT ?`, func(rows *sql.Rows) (store.MediaDeleteRevertCandidate, error) {
		var c store.MediaDeleteRevertCandidate
		err := rows.Scan(&c.ID, &c.MediaAssetID, &c.RecordingID, &c.RelPath, &c.Kind)
		return c, err
	}, all...)
	if err != nil {
		return nil, fmt.Errorf("listing unqualified deleting media asset copies: %w", err)
	}
	return rows, nil
}

// ListOldGenerationMediaAssetCopiesToDelete は overlap を過ぎた旧世代コピーを返す。
func (s *Media) ListOldGenerationMediaAssetCopiesToDelete(ctx context.Context, overlapCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := queryRows(ctx, s.db, "old-generation media asset copies", `
SELECT o.id, a.id, a.recording_id, o.rel_path, a.size_bytes, a.kind, o.generation
FROM old_generation_media_asset_copies o
JOIN media_assets a ON a.id = o.media_asset_id
WHERE o.store = ? AND o.state = 'active' AND o.generation_started_at <= ?
ORDER BY o.id
LIMIT ?`, scanMediaDeleteAsset, s.storeName, overlapCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("listing old-generation media asset copies: %w", err)
	}
	return rows, nil
}

// ListTrashAssetsToDelete はごみ箱の猶予を過ぎたか即時削除を要求されたコピーを返す。
func (s *Media) ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	return s.listMediaDeleteAssets(ctx, "trash media asset copies to delete", "active", trashDeletableAssetSQL, []any{graceCutoff}, limit)
}

// ListUntilEncodedOriginalsToDelete は派生物が揃った until_encoded 原本コピーを返す。
func (s *Media) ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	return s.listMediaDeleteAssets(ctx, "until_encoded original copies to delete", "active", untilEncodedDeletableSQL, untilEncodedArgs(s.desiredStore()), limit)
}

// ListRemovedEncodedAssetsToDelete は利用者が外した encoded コピーを返す。
func (s *Media) ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	return s.listMediaDeleteAssets(ctx, "removed encoded asset copies to delete", "active", removedEncodedAssetSQL, nil, limit)
}

// MarkDeleting は active コピーを deleting にする。
func (s *Media) MarkDeleting(ctx context.Context, copyID int64, expectedState string) (bool, error) {
	marked := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		locked, found, err := lockMediaDeleteCopy(ctx, tx, copyID, s.storeName)
		if err != nil {
			return err
		}
		if !found || locked.state != expectedState {
			return nil
		}
		n, err := setMediaAssetCopyState(ctx, tx, copyID, s.storeName, "deleting", expectedState, nil)
		if err != nil {
			return fmt.Errorf("marking media asset copy %d deleting: %w", copyID, err)
		}
		marked = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}

// MarkUntilEncodedDeleting は録画の行をロックして削除条件を再確認し、満たせば原本のコピーを deleting にする。
// 消去済みの録画は何もせず、ごみ箱の録画は store.ErrTrashed を返す（pgstore の lockRecording と同じ）。
// 判定は録画の行ロックの下で SELECT し、UPDATE は判定の後に copy の id で行う（UPDATE の副問い合わせは対象表を読めない）。
func (s *Media) MarkUntilEncodedDeleting(ctx context.Context, recordingID, mediaAssetID, copyID int64) (bool, error) {
	marked := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		trashed, purged, err := lockRecordingFlags(ctx, tx, recordingID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking recording %d before until_encoded delete: %w", recordingID, err)
		}
		if purged {
			return nil
		}
		if trashed {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrTrashed)
		}
		locked, found, err := lockMediaDeleteCopy(ctx, tx, copyID, s.storeName)
		if err != nil {
			return err
		}
		if !found || locked.mediaAssetID != mediaAssetID || locked.state != "active" {
			return nil
		}
		var eligible bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM media_assets a WHERE a.id = ? AND `+untilEncodedDeletableSQL+`)`,
			append([]any{mediaAssetID}, untilEncodedArgs(s.desiredStore())...)...).Scan(&eligible); err != nil {
			return fmt.Errorf("rechecking until_encoded original %d: %w", mediaAssetID, err)
		}
		if !eligible {
			return nil
		}
		n, err := setMediaAssetCopyState(ctx, tx, copyID, s.storeName, "deleting", "active", nil)
		if err != nil {
			return fmt.Errorf("marking until_encoded original copy %d deleting: %w", copyID, err)
		}
		marked = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}

// MarkDeleted は deleting コピーを deleted にし、全コピーの削除後に encoded 削除要求を消す。
func (s *Media) MarkDeleted(ctx context.Context, copyID int64) error {
	return s.markCopyDeleted(ctx, copyID, s.storeName)
}

// markCopyDeleted は storeName の置き場にある copyID の deleting コピーを deleted にする。
// 実体無しの記録と転送の試行行は、コピーが deleted になるのと同じ tx で消す。
// 外した版の要求は、同じ版の全コピーが deleted になったときだけ消す。
func (s *Media) markCopyDeleted(ctx context.Context, copyID int64, storeName string) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		locked, found, err := lockMediaDeleteCopy(ctx, tx, copyID, storeName)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if locked.state != "deleted" {
			if locked.state != "deleting" {
				return fmt.Errorf("media asset copy %d has state %q, want deleting or deleted", copyID, locked.state)
			}
			n, err := setMediaAssetCopyState(ctx, tx, copyID, storeName, "deleted", "deleting", nil)
			if err != nil {
				return fmt.Errorf("marking media asset copy %d deleted: %w", copyID, err)
			}
			if n != 1 {
				return fmt.Errorf("marking media asset copy %d deleted updated %d rows, want one", copyID, n)
			}
		}
		for _, stmt := range []string{
			`DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`,
			`DELETE FROM media_asset_transfer_attempts WHERE copy_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, copyID); err != nil {
				return fmt.Errorf("clearing state of deleted copy %d: %w", copyID, err)
			}
		}
		// 要求は論理資産の主張なので、全コピーが deleted になるまで残す（pgstore の ClearDeletedEncodedAssetRemovalRequest と同じ）。
		// 複数表の DELETE で要求の表だけを消す。副問い合わせは media_asset_copies を読むが、そちらは消さない。
		if _, err := tx.ExecContext(ctx, `
DELETE q
FROM encoded_asset_removal_requests q
JOIN media_assets a ON a.recording_id = q.recording_id AND a.profile = q.profile AND a.kind = 'encoded'
JOIN media_asset_copies c0 ON c0.media_asset_id = a.id AND c0.id = ?
WHERE NOT EXISTS (
    SELECT 1 FROM media_asset_copies copy
    WHERE copy.media_asset_id = a.id AND copy.state <> 'deleted'
)`, copyID); err != nil {
			return fmt.Errorf("clearing completed encoded asset removal request for copy %d: %w", copyID, err)
		}
		return nil
	})
}

// RevertToActive は削除条件から外れた deleting コピーを active に戻す。
// 判定は行ロックの下で SELECT し、満たせば戻さない。UPDATE は copy の id と状態で行う。
func (s *Media) RevertToActive(ctx context.Context, copyID int64, graceCutoff, overlapCutoff time.Time) (bool, error) {
	arms, err := s.deletionArms(graceCutoff, overlapCutoff)
	if err != nil {
		return false, err
	}
	where, args := joinArms(arms)
	reverted := false
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		locked, found, err := lockMediaDeleteCopy(ctx, tx, copyID, s.storeName)
		if err != nil {
			return err
		}
		if !found || locked.state != "deleting" {
			return nil
		}
		var unqualified bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
    WHERE c.id = ? AND c.store = ? AND c.state = 'deleting' AND NOT `+where+`
)`, append([]any{copyID, s.storeName}, args...)...).Scan(&unqualified); err != nil {
			return fmt.Errorf("rechecking deleting media asset copy %d: %w", copyID, err)
		}
		if !unqualified {
			return nil
		}
		n, err := setMediaAssetCopyState(ctx, tx, copyID, s.storeName, "active", "deleting", nil)
		if err != nil {
			return fmt.Errorf("reverting media asset copy %d to active: %w", copyID, err)
		}
		reverted = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return reverted, nil
}

// lockedMediaDeleteCopy はロックしたコピー行の、削除の判定に要る列である。
type lockedMediaDeleteCopy struct {
	mediaAssetID int64
	state        string
}

// lockMediaDeleteCopy は storeName の置き場の copyID を FOR UPDATE で取る。無ければ found は false。
// 判定と遷移を同じ行ロックの下で行うため、遷移の前に必ず呼ぶ（pgstore の lockMediaAssetCopy と同じ役）。
func lockMediaDeleteCopy(ctx context.Context, tx *sql.Tx, copyID int64, storeName string) (lockedMediaDeleteCopy, bool, error) {
	var c lockedMediaDeleteCopy
	err := tx.QueryRowContext(ctx, `
SELECT media_asset_id, state FROM media_asset_copies WHERE id = ? AND store = ? FOR UPDATE`, copyID, storeName).Scan(&c.mediaAssetID, &c.state)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("locking media asset copy %d: %w", copyID, err)
	}
	return c, true, nil
}

// originalStoreRuleValue と originalStoreMatchValue は、配置規則を prunableCopySQL が読む JSON の形である。
// pgstore の同名の型と同じ形（store・match.site・match.older_than_us）。
type originalStoreRuleValue struct {
	Match *originalStoreMatchValue `json:"match,omitempty"`
	Store string                   `json:"store"`
}

type originalStoreMatchValue struct {
	Site            string `json:"site,omitempty"`
	OlderThanMicros int64  `json:"older_than_us,omitempty"`
}

// originalRulesJSON は方針の原本の規則を JSON 配列にする。pgstore の originalRulesJSON と同じ判定。
func originalRulesJSON(policy store.StoragePolicy) ([]byte, error) {
	rules := policy.OriginalRules()
	encodedRules := make([]originalStoreRuleValue, 0, len(rules))
	for i, rule := range rules {
		if rule.OlderThan < 0 || rule.OlderThan%time.Microsecond != 0 {
			return nil, fmt.Errorf("original rule %d older-than duration must have microsecond precision", i)
		}
		converted := originalStoreRuleValue{Store: rule.Store}
		if rule.Site != "" || rule.OlderThan != 0 {
			match := &originalStoreMatchValue{Site: rule.Site}
			if rule.OlderThan != 0 {
				match.OlderThanMicros = rule.OlderThan.Microseconds()
			}
			converted.Match = match
		}
		encodedRules = append(encodedRules, converted)
	}
	encoded, err := json.Marshal(encodedRules)
	if err != nil {
		return nil, fmt.Errorf("encoding original store rules: %w", err)
	}
	return encoded, nil
}
