-- +goose Up

-- LGD does not contain the resolution it was learned from, so existing rows
-- cannot be backfilled safely. They must be learned again with an observed size.
DELETE FROM cm_logos;

ALTER TABLE cm_logos
    ADD COLUMN coded_width integer,
    ADD COLUMN coded_height integer;

ALTER TABLE cm_logos
    ALTER COLUMN coded_width SET NOT NULL,
    ALTER COLUMN coded_height SET NOT NULL,
    ADD CONSTRAINT cm_logos_coded_size_check CHECK (coded_width > 0 AND coded_height > 0);

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_stage_check,
    ADD CONSTRAINT recording_cm_attempts_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save',
            'stopped', 'resolution', 'match'
        ]::text[])
    );

-- +goose Down

ALTER TABLE cm_logos
    DROP CONSTRAINT cm_logos_coded_size_check,
    DROP COLUMN coded_width,
    DROP COLUMN coded_height;

-- 旧 CHECK は新しい 2 値を許さない。再追加の前に、該当する試行の工程を未記録へ戻す。
UPDATE recording_cm_attempts SET stage = NULL WHERE stage IN ('resolution', 'match');

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_stage_check,
    ADD CONSTRAINT recording_cm_attempts_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save', 'stopped'
        ]::text[])
    );
