-- +goose Up

ALTER TABLE public.recordings
    DROP CONSTRAINT recordings_source_check;

ALTER TABLE public.recordings
    ADD CONSTRAINT recordings_source_check
    CHECK ((source = ANY (ARRAY['rule'::text, 'manual'::text, 'unattributed'::text])));

-- +goose Down

ALTER TABLE public.recordings
    DROP CONSTRAINT recordings_source_check;

ALTER TABLE public.recordings
    ADD CONSTRAINT recordings_source_check
    CHECK ((source = ANY (ARRAY['rule'::text, 'manual'::text])));
