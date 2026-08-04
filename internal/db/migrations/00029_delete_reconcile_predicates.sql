-- +goose Up

-- issue #160: 削除 reconcile（internal/db/queries/delete_reconcile.sql）の
-- 「このアセットは消してよいか」の述語 ——「ごみ箱の猶予超過 or 今すぐ
-- purge」と「until_encoded の派生物完備」—— が 5 クエリ（入口 2 つ・pending
-- の拾い直し・否定形 2 つ）に複製され、うち 1 つ（until_encoded の入口）だけが
-- issue #104 の cardinality(encode_profiles) > 0 ガードを持つという形でドリフト
-- していた。両腕にスキーマ上の名前を与え、5 複製をこの名前への参照に統一する。

-- until_encoded 腕: パラメータを取らないので view にできる。原本アセット
-- （kind = 'original'）のうち、保持ポリシーが until_encoded かつ望ましい
-- 派生物（全 encode_profiles + thumbnail）がすべて active でコミット済みの
-- ものを列挙する。a.state（active / deleting）はこの述語自体には関係しない
-- —— 呼び出し側が「まだ消していない」（入口）か「deleting のまま止まって
-- いる」（pending / revert）かで a.state を絞る。
--
-- cardinality(r.encode_profiles) > 0 は load-bearing（issue #104）。これを
-- 欠くと NOT EXISTS(unnest(encode_profiles) ...) が空配列で恒真になり、
-- サムネイルが 1 枚あるだけで唯一のコピーである原本が消える
-- （docs/storage.md §6「唯一のコピーを消すパスがない」への違反）。定義を
-- ここ 1 箇所に集約することで、この後 pending / unqualified / revert が
-- それぞれ手で複製していたためにガードが漏れていた 3 経路にも自動的に効く。
CREATE VIEW until_encoded_deletable_originals AS
SELECT
    a.id           AS asset_id,
    a.recording_id AS recording_id,
    a.rel_path     AS rel_path,
    a.size_bytes   AS size_bytes,
    a.state        AS state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.kind = 'original'
  AND r.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(r.encode_profiles) > 0
  AND NOT EXISTS (
    SELECT 1 FROM unnest(r.encode_profiles) AS want(profile)
    WHERE NOT EXISTS (
      SELECT 1 FROM media_assets e
      WHERE e.recording_id = r.id
        AND e.kind = 'encoded'
        AND e.state = 'active'
        AND e.profile = want.profile
    )
  )
  AND EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
  );

-- ごみ箱腕: grace_cutoff がパラメータなので view に畳めない（view は引数を
-- 取れない）。set-returning SQL 関数にする —— sqlc は関数への SELECT を
-- 扱える。ごみ箱の猶予超過、または「今すぐ完全削除」（purge_after）の対象と
-- なる録画の id を返す。
CREATE FUNCTION trash_deletable_recordings(grace_cutoff timestamptz)
RETURNS TABLE (recording_id bigint)
LANGUAGE sql STABLE AS $$
    SELECT r.id
    FROM recordings r
    WHERE r.deleted_at IS NOT NULL
      AND (
        (r.purge_after IS NOT NULL AND r.purge_after <= now())
        OR r.deleted_at <= grace_cutoff
      )
$$;

-- +goose Down
DROP FUNCTION IF EXISTS trash_deletable_recordings(timestamptz);
DROP VIEW IF EXISTS until_encoded_deletable_originals;
