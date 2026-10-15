-- +goose Up

-- 行の存在が「利用者がこの keep 区間でカット版を作り直したい」という未充足の
-- 命令である。現在のチャプターから導出できる cutStale とは別の寿命を持つため、
-- media_assets の子ではなく録画・profile をキーにする衛星表にする。
CREATE SEQUENCE encoded_asset_rebuild_request_generation_seq AS bigint;

CREATE TABLE encoded_asset_rebuild_requests (
    recording_id  bigint NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
    profile       text NOT NULL,
    requested_keep int8multirange NOT NULL CHECK (NOT isempty(requested_keep)),
    request_generation bigint NOT NULL DEFAULT nextval('encoded_asset_rebuild_request_generation_seq') CHECK (request_generation > 0),
    requested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recording_id, profile)
);
ALTER SEQUENCE encoded_asset_rebuild_request_generation_seq
    OWNED BY encoded_asset_rebuild_requests.request_generation;

COMMENT ON TABLE encoded_asset_rebuild_requests IS
    '利用者が明示したカット版の再作成要求。requested_keep は操作時点の量子化済み区間。catalog には含めない。';

-- encodable_recordings は「エンコードしてよい録画」の録画単位の述語である。
-- ごみ箱の録画は作ってはならず、原本が active でない録画は入力が無いので作れない。
-- encode の不足の補完（ListMissingEncodeProfiles）・待機列（ListEncodeQueue）・
-- 作り直し要求の列挙（ListEncodedAssetRebuildRequests）と、それが落とした側を
-- 数えるメトリクス（ListUnsatisfiableEncodeProfiles / ListCutAwaitingReview）が
-- この view を引く。条件を足すときはここだけを変える（写しの片方だけを直すと、
-- 待機列に出るのに投入されない・投入されるのに待機列に出ない、が起きる）。
-- 原本を消してよいかの until_encoded_deletable_originals は別の判定で、これを使わない。
CREATE VIEW encodable_recordings AS
SELECT r.id AS recording_id
FROM recordings r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM media_assets o
      WHERE o.recording_id = r.id
        AND o.kind = 'original'
        AND o.state = 'active'
  );

COMMENT ON VIEW encodable_recordings IS
    'EncodableRecording は、ごみ箱になく原本が active な録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。';

-- encodable_profile は「エンコードしてよいプロファイル」のプロファイル単位の述語
-- である。空文字でなく、現在の設定（known_profiles）にあり、cut_profiles に含まれる
-- なら利用者がチャプターを確認済み（recording_chapter_ownership の行がある）。
-- 設定値を引数に取るので view にはできない。使う側は不足の補完
-- （ListMissingEncodeProfiles）と待機列（ListEncodeQueue）である。作り直し要求の
-- 列挙（ListEncodedAssetRebuildRequests）はこれで絞らない。確認が外れた要求を
-- reconcile が古い要求として消すには、その要求を読む必要があるためである。
--
-- known_profiles は non-NULL でなければならない。`x = ANY(NULL::text[])` は false
-- ではなく NULL なので、NULL を渡すとこの関数は常に NULL を返し、WHERE で候補が
-- 全滅する。関数の引数にしても SQL の 3 値論理はそのまま通る（STRICT にしていない
-- ので、NULL の扱いはインラインで書いた式と同じ）。呼び出し側は
-- config.EncodeConfig.ProfileNames()（空設定でも non-nil）を渡す。
CREATE FUNCTION encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[])
RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT profile_name <> ''
       AND profile_name = ANY(known_profiles)
       AND (
           NOT (profile_name = ANY(cut_profiles))
           OR EXISTS (
               SELECT 1 FROM recording_chapter_ownership o
               WHERE o.recording_id = rec_id
           )
       )
$$;

COMMENT ON FUNCTION encodable_profile(bigint, text, text[], text[]) IS
    'encodable_profile は、録画 rec_id のプロファイル profile_name が現在の設定で作れるかを返す。cut プロファイルはチャプター確認済みを要求する。known_profiles に NULL を渡すと NULL を返す。';

-- 未充足の作り直し命令がある間は原本を保持する。要求が stale になれば
-- encode_reconcile が比較交換で要求行を消し、次の削除パスで原本を再判定する。
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
      SELECT 1
      FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = want.profile
      )
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
          SELECT 1 FROM recording_cm_attempts ca
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
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = want.profile
      )
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
          SELECT 1 FROM recording_cm_attempts ca
          WHERE ca.recording_id = r.id
            AND ca.state = 'failed'
            AND ca.stage IS DISTINCT FROM 'adopt'
      )
  );

DROP FUNCTION encodable_profile(bigint, text, text[], text[]);
DROP VIEW encodable_recordings;
DROP TABLE encoded_asset_rebuild_requests;
