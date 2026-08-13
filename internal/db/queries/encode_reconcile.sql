-- encode の desired−observed 定期 reconcile（internal/worker/encode_reconcile.go）が
-- 読む候補クエリ。record_sweep（watcher の定期全量突き合わせ）とは対象集合が違う
-- ので、クエリを共有せず新規に切る --- record_sweep が見るのは mirakc のエッジに
-- 残っている record（DB の外の観測）で、こちらは「原本をコミット済みなのに
-- desired なプロファイルの encoded が揃っていない録画」（DB だけで閉じる）。

-- 「原本が active でコミット済み、かつ desired なプロファイルのうち少なくとも 1 つ
-- について active な encoded が無い」録画を返す。
--
-- 条件の意味:
--   * recording_encode_policy に行がある = エンコードポリシーが凍結済み
--     （行が無い = 未凍結。不変条件 10。JOIN で自然に落ちる）
--   * cardinality(encode_profiles) > 0 = desired が空でない
--   * 原本 media_assets（kind='original', state='active'）の EXISTS = ingest
--     コミット済み。ingest 未完了の録画を対象にしない（issue #163 の「含むもの」2）。
--     state='active' まで見るのは EnqueueMissingEncodes 側の判定
--     （GetActiveOriginalMediaAsset）と一致させるため --- 原本が until_encoded で
--     物理削除済みの録画をここで候補に挙げても、EnqueueMissingEncodes が
--     no-op を返すだけで前進しない
--   * r.deleted_at IS NULL = ごみ箱の録画は対象外。ヒント経路（ingest 完了 /
--     POST /api/recordings/{id}/encode-profiles）は「今その録画に何かが起きた」
--     という個別のイベントで発火するが、この定期パスは全録画を毎回なめるので、
--     ユーザーが捨てた録画のエンコードを延々と再投入し続けることになる。
--     until_encoded_deletable_originals（00032）が同じ述語を持つのと同じ理由
--   * 空文字列のプロファイル名は候補の判定から除く。EnqueueMissingEncodes が
--     空文字列をスキップするので、候補の定義もそれに揃える（揃えないと、空文字列
--     だけを desired に持つ録画が毎パス候補に挙がっては何も投入されない、を繰り返す）
--
-- name: ListRecordingsMissingEncodes :many
SELECT p.recording_id
FROM recording_encode_policy p
JOIN recordings r ON r.id = p.recording_id
WHERE cardinality(p.encode_profiles) > 0
  AND r.deleted_at IS NULL
  AND EXISTS (
    SELECT 1 FROM media_assets o
    WHERE o.recording_id = p.recording_id
      AND o.kind = 'original'
      AND o.state = 'active'
  )
  AND EXISTS (
    SELECT 1 FROM unnest(p.encode_profiles) AS want(profile)
    WHERE want.profile <> ''
      AND NOT EXISTS (
        SELECT 1 FROM media_assets e
        WHERE e.recording_id = p.recording_id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile = want.profile
      )
  )
ORDER BY p.recording_id
LIMIT sqlc.arg('row_limit');
