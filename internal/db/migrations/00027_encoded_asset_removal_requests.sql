-- +goose Up

-- 行の存在が「ユーザーがこの版を外した」という主張である（不変条件 10。
-- recording_purge_requests と同じ形）。行は asset の deleted 確定と、
-- 同じ profile の足し直しで、それぞれ同じ tx の中で消す。
CREATE TABLE encoded_asset_removal_requests (
    recording_id bigint NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
    profile      text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recording_id, profile)
);

-- 削除 reconcile の 3 つ目の腕。「desired に無い」だけで消すと、rescue などで
-- desired と実ファイルがずれた録画の版を黙って消すので、要求行を必須にする。
-- 足し直しで desired に戻った版は、要求行が残っていても消さない。
--
-- 他の版が残ることも条件にする。API の 0 コピー検査はこの view を参照する。
-- 削除 reconcile は列挙した until_encoded 原本を述語の再評価なしで unlink するので、
-- API が原本ありを見て通した後に原本が消えうる。その場合も最後の版は消さない。
CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN encoded_asset_removal_requests q
    ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND other.state = 'active'
        AND (
            other.kind = 'original'
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
            )
        )
  );

-- +goose Down

DROP VIEW removed_encoded_assets;
DROP TABLE encoded_asset_removal_requests;
