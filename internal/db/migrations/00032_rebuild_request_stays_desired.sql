-- +goose Up

-- encoded_asset_rebuild_requests の行は、満たされても消さない「利用者が押した時点の
-- keep」という desired である。消すのは api だけ（版を外す）。満たされたかは行に
-- 持たず、active な encoded の凍結区間（media_asset_cuts）との一致から導出する
-- （不変条件 9）。
--
-- 表の名前は変えない。置き換え前の Pod が同じ名前を引き続けるので、改名は
-- ローリング更新の間の書き込み失敗になる。「要求」は利用者の要求そのもので、満たされた
-- 後も「利用者が確認した keep」の記録として残る。
--
-- request_generation 列とシーケンスは落とす。満たされた要求を消す書き手が無くなり、
-- 消費と削除の競合を防ぐ比較交換が要らなくなった。シーケンスは列の OWNED BY なので
-- 列と一緒に落ちる。ローリング更新の間、置き換え前の Pod の upsert（nextval）と
-- 比較交換削除、ListEncodedAssetRebuildRequests（列を読む）は失敗する。
-- 新しい Pod に置き換われば解消する。
ALTER TABLE encoded_asset_rebuild_requests DROP COLUMN request_generation;

-- unsatisfied_encode_rebuilds は「満たされていない作り直し」の述語である。
-- ごみ箱の録画と原本が active でない録画は含まない（encodable_recordings）。
-- 満たされている = 同じプロファイルの active な encoded の凍結区間が requested_keep と
-- 等しい（int8multirange の =。区間は書き込み時に量子化・正規化済み）。
-- ListMissingEncodeProfiles / ListEncodeQueue / ListEncodedAssetRebuildRequests と
-- until_encoded_deletable_originals が引く。条件を足すときはここだけを変える。
CREATE VIEW unsatisfied_encode_rebuilds AS
SELECT q.recording_id,
       q.profile,
       q.requested_keep
FROM encoded_asset_rebuild_requests q
JOIN encodable_recordings er ON er.recording_id = q.recording_id
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN media_asset_cuts c ON c.media_asset_id = e.id
    WHERE e.recording_id = q.recording_id
      AND e.kind = 'encoded'
      AND e.state = 'active'
      AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
);

COMMENT ON VIEW unsatisfied_encode_rebuilds IS
    'unsatisfied_encode_rebuilds は、requested_keep と一致する凍結区間の active な encoded がまだ無い作り直し要求を表す。ごみ箱・原本が active でない録画は含まない。';

COMMENT ON TABLE encoded_asset_rebuild_requests IS
    '利用者が押した時点の keep 区間（量子化済み）。満たされても残る desired で、消すのは api だけ。満たされたかは unsatisfied_encode_rebuilds が導出する。catalog には含めない。';

-- encodable_profile の cut の確認条件（recording_chapter_ownership の行）は、作り直しには
-- 掛けない。作り直しは押した時点の keep（requested_keep）で描き、その keep そのものが
-- 利用者の確認だからである。掛けるのは、チャプターから導出する初回のカット版
-- （unencoded_desired_profiles 由来）だけで、ListEncodeQueue は作り直しの側を
-- encodable_profile に通さない。
COMMENT ON FUNCTION encodable_profile(bigint, text, text[], text[]) IS
    'encodable_profile は、録画 rec_id のプロファイル profile_name が現在の設定で作れるかを返す。cut プロファイルはチャプター確認済みを要求する。ただし作り直し要求（押した時点の keep が確認）には使わない。known_profiles に NULL を渡すと NULL を返す。';

-- 未充足の作り直しがある間は原本を保持する。満たされれば（または api が行を消せば）
-- 次の削除パスで原本を再判定する。
CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE a.kind = 'original'
  AND p.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(p.encode_profiles) > 0
  AND NOT EXISTS (
      SELECT 1 FROM unsatisfied_encode_rebuilds q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM unencoded_desired_profiles u
      WHERE u.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1 FROM media_assets t
      WHERE t.recording_id = r.id
        AND t.kind = 'thumbnail'
        AND t.state = 'active'
  )
  AND EXISTS (
      SELECT 1 FROM media_assets s
      WHERE s.recording_id = r.id
        AND s.kind = 'seek_tiles'
        AND s.state = 'active'
  )
  AND (
      NOT p.cm_detect
      OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
      OR EXISTS (
          SELECT 1 FROM current_cm_attempts ca
          WHERE ca.recording_id = r.id
            AND ca.state = 'failed'
            AND ca.stage IS DISTINCT FROM 'adopt'
      )
  );

-- +goose Down

CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE a.kind = 'original'
  AND p.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(p.encode_profiles) > 0
  AND NOT EXISTS (
      SELECT 1 FROM encoded_asset_rebuild_requests q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM unencoded_desired_profiles u
      WHERE u.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1 FROM media_assets t
      WHERE t.recording_id = r.id
        AND t.kind = 'thumbnail'
        AND t.state = 'active'
  )
  AND EXISTS (
      SELECT 1 FROM media_assets s
      WHERE s.recording_id = r.id
        AND s.kind = 'seek_tiles'
        AND s.state = 'active'
  )
  AND (
      NOT p.cm_detect
      OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
      OR EXISTS (
          SELECT 1 FROM current_cm_attempts ca
          WHERE ca.recording_id = r.id
            AND ca.state = 'failed'
            AND ca.stage IS DISTINCT FROM 'adopt'
      )
  );

DROP VIEW unsatisfied_encode_rebuilds;

CREATE SEQUENCE encoded_asset_rebuild_request_generation_seq AS bigint;
ALTER TABLE encoded_asset_rebuild_requests
    ADD COLUMN request_generation bigint NOT NULL DEFAULT nextval('encoded_asset_rebuild_request_generation_seq') CHECK (request_generation > 0);
ALTER SEQUENCE encoded_asset_rebuild_request_generation_seq
    OWNED BY encoded_asset_rebuild_requests.request_generation;

COMMENT ON TABLE encoded_asset_rebuild_requests IS
    '利用者が明示したカット版の再作成要求。requested_keep は操作時点の量子化済み区間。catalog には含めない。';
