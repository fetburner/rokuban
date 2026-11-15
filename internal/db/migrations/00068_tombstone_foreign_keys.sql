-- +goose Up
-- tombstone の親を誤って DELETE しても、衛星行や参照を失わないようにする。
-- DROP CONSTRAINT は子表の ACCESS EXCLUSIVE を取り、ADD FOREIGN KEY は子表全体を検証する。
-- ロックはトランザクション終了まで保持するので、同じトランザクション内で NOT VALID と
-- VALIDATE に分けても占有期間は縮まらない。検証完了までのアクセス待ちを受け入れ、分割しない。
-- Compose は別の migrate サービスの完了後に本体を起動する（docker-compose.yml）。
-- k8s も別 Job だが、base の Deployment は同時に起動し得る（deploy/k8s/README.md「使い方」）。
-- 別プロセスだから競合しないとは扱わず、稼働中ロールのアクセスも待たせ得る移行として適用する。
-- ロックと検証の根拠: PostgreSQL 17 の ALTER TABLE / Explicit Locking の公開仕様。
-- https://www.postgresql.org/docs/17/sql-altertable.html
-- https://www.postgresql.org/docs/17/explicit-locking.html
-- 所要時間は未計測。無停止での適用や Job の期限内の完了は保証しない。
ALTER TABLE cm_logo_candidates
    DROP CONSTRAINT cm_logo_candidates_recording_id_fkey,
    ADD CONSTRAINT cm_logo_candidates_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE cm_logos
    DROP CONSTRAINT cm_logos_learned_from_fkey,
    ADD CONSTRAINT cm_logos_learned_from_fkey
        FOREIGN KEY (learned_from) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE encoded_asset_rebuild_requests
    DROP CONSTRAINT encoded_asset_rebuild_requests_recording_id_fkey,
    ADD CONSTRAINT encoded_asset_rebuild_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE encoded_asset_removal_requests
    DROP CONSTRAINT encoded_asset_removal_requests_recording_id_fkey,
    ADD CONSTRAINT encoded_asset_removal_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE label_rule_hits
    DROP CONSTRAINT label_rule_hits_recording_id_fkey,
    ADD CONSTRAINT label_rule_hits_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_copies_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE NO ACTION;

ALTER TABLE media_asset_cuts
    DROP CONSTRAINT media_asset_cuts_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_cuts_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE NO ACTION;

ALTER TABLE media_asset_thumbnail_seeks
    DROP CONSTRAINT media_asset_thumbnail_seeks_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_thumbnail_seeks_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE NO ACTION;

ALTER TABLE media_asset_ts_scans
    DROP CONSTRAINT media_asset_ts_scans_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_ts_scans_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE NO ACTION;

ALTER TABLE record_sync
    DROP CONSTRAINT record_sync_recording_id_fkey,
    ADD CONSTRAINT record_sync_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_chapter_ownership
    DROP CONSTRAINT recording_chapter_ownership_recording_id_fkey,
    ADD CONSTRAINT recording_chapter_ownership_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_recording_id_fkey,
    ADD CONSTRAINT recording_cm_attempts_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_cm_detections
    DROP CONSTRAINT recording_cm_detections_recording_id_fkey,
    ADD CONSTRAINT recording_cm_detections_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_encode_attempts
    DROP CONSTRAINT recording_encode_attempts_recording_id_fkey,
    ADD CONSTRAINT recording_encode_attempts_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_processing_policy
    DROP CONSTRAINT recording_processing_policy_recording_id_fkey,
    ADD CONSTRAINT recording_processing_policy_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_ingest_checkpoints
    DROP CONSTRAINT recording_ingest_checkpoints_recording_id_fkey,
    ADD CONSTRAINT recording_ingest_checkpoints_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_ingest_progress
    DROP CONSTRAINT recording_ingest_progress_recording_id_fkey,
    ADD CONSTRAINT recording_ingest_progress_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

ALTER TABLE recording_purge_requests
    DROP CONSTRAINT recording_purge_requests_recording_id_fkey,
    ADD CONSTRAINT recording_purge_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE NO ACTION;

-- +goose Down
ALTER TABLE cm_logo_candidates
    DROP CONSTRAINT cm_logo_candidates_recording_id_fkey,
    ADD CONSTRAINT cm_logo_candidates_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE SET NULL;

ALTER TABLE cm_logos
    DROP CONSTRAINT cm_logos_learned_from_fkey,
    ADD CONSTRAINT cm_logos_learned_from_fkey
        FOREIGN KEY (learned_from) REFERENCES recordings (id) ON DELETE SET NULL;

ALTER TABLE encoded_asset_rebuild_requests
    DROP CONSTRAINT encoded_asset_rebuild_requests_recording_id_fkey,
    ADD CONSTRAINT encoded_asset_rebuild_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE encoded_asset_removal_requests
    DROP CONSTRAINT encoded_asset_removal_requests_recording_id_fkey,
    ADD CONSTRAINT encoded_asset_removal_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE label_rule_hits
    DROP CONSTRAINT label_rule_hits_recording_id_fkey,
    ADD CONSTRAINT label_rule_hits_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_copies_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE;

ALTER TABLE media_asset_cuts
    DROP CONSTRAINT media_asset_cuts_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_cuts_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE;

ALTER TABLE media_asset_thumbnail_seeks
    DROP CONSTRAINT media_asset_thumbnail_seeks_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_thumbnail_seeks_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE;

ALTER TABLE media_asset_ts_scans
    DROP CONSTRAINT media_asset_ts_scans_media_asset_id_fkey,
    ADD CONSTRAINT media_asset_ts_scans_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE;

ALTER TABLE record_sync
    DROP CONSTRAINT record_sync_recording_id_fkey,
    ADD CONSTRAINT record_sync_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE SET NULL;

ALTER TABLE recording_chapter_ownership
    DROP CONSTRAINT recording_chapter_ownership_recording_id_fkey,
    ADD CONSTRAINT recording_chapter_ownership_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_recording_id_fkey,
    ADD CONSTRAINT recording_cm_attempts_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_cm_detections
    DROP CONSTRAINT recording_cm_detections_recording_id_fkey,
    ADD CONSTRAINT recording_cm_detections_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_encode_attempts
    DROP CONSTRAINT recording_encode_attempts_recording_id_fkey,
    ADD CONSTRAINT recording_encode_attempts_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_processing_policy
    DROP CONSTRAINT recording_processing_policy_recording_id_fkey,
    ADD CONSTRAINT recording_processing_policy_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_ingest_checkpoints
    DROP CONSTRAINT recording_ingest_checkpoints_recording_id_fkey,
    ADD CONSTRAINT recording_ingest_checkpoints_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_ingest_progress
    DROP CONSTRAINT recording_ingest_progress_recording_id_fkey,
    ADD CONSTRAINT recording_ingest_progress_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;

ALTER TABLE recording_purge_requests
    DROP CONSTRAINT recording_purge_requests_recording_id_fkey,
    ADD CONSTRAINT recording_purge_requests_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES recordings (id) ON DELETE CASCADE;
