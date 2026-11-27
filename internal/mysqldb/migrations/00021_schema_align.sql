-- +goose Up
-- Postgres の最終形（internal/db/schema.sql）に揃える。既存の migration のスキーマは書き換えず、差分だけをここで直す。

-- 転送中に作った置き場のキーの一意性を DB が守る。rel_path は LONGTEXT なので、主キーには
-- orphan_files と同じ SHA-256 の生成列を使う（バイト単位の比較）。copy_id の索引は主キーの先頭で足りる。
ALTER TABLE media_asset_transfer_objects
    ADD COLUMN rel_path_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(rel_path, 256))) STORED AFTER rel_path,
    ADD PRIMARY KEY (copy_id, rel_path_key);
ALTER TABLE media_asset_transfer_objects DROP INDEX media_asset_transfer_objects_copy_id;

-- 区間の開始位置の一意性を DB が守る。区間の重なり全体は表が拒めないので、書き手が確かめる（store-contracts の CM 検出とチャプター編集）。
ALTER TABLE recording_chapter_spans DROP INDEX recording_chapter_spans_recording,
    ADD PRIMARY KEY (recording_id, start_ms);

-- 時刻は DATETIME(6) に揃える（TIMESTAMP は 2038-01-19 より後を表せない）。
ALTER TABLE circuit_breakers MODIFY tripped_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6);

-- 録画の一覧（program_start_at・id の並び）と、局単位の CM 検出の問い合わせに索引を足す。
-- 録画 5000 件を入れた MySQL 9.7 の EXPLAIN で、並べ替えの filesort と全件走査が消えたものだけを入れた。
-- 「続きから」の並び（playback の updated_at）は、EXPLAIN で計画が変わらなかったので足さない。
ALTER TABLE recordings
    ADD INDEX recordings_program_start_at_id_idx (program_start_at DESC, id DESC),
    ADD INDEX recordings_broadcast_event_idx (network_id, service_id, program_start_at);

-- +goose Down
ALTER TABLE recordings DROP INDEX recordings_broadcast_event_idx, DROP INDEX recordings_program_start_at_id_idx;
-- 2038-01-19 より後の tripped_at があると、TIMESTAMP への戻しは範囲外で失敗する。
ALTER TABLE circuit_breakers MODIFY tripped_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6);

ALTER TABLE recording_chapter_spans ADD KEY recording_chapter_spans_recording (recording_id, start_ms);
ALTER TABLE recording_chapter_spans DROP PRIMARY KEY;

ALTER TABLE media_asset_transfer_objects ADD KEY media_asset_transfer_objects_copy_id (copy_id);
ALTER TABLE media_asset_transfer_objects DROP PRIMARY KEY, DROP COLUMN rel_path_key;
