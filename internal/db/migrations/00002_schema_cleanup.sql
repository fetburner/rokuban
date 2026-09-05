-- +goose Up

-- recording_encode_policy は recordings の寿命に従う衛星表（issue #641）。
ALTER TABLE public.recording_encode_policy
    DROP CONSTRAINT recording_encode_policy_recording_id_fkey;

ALTER TABLE public.recording_encode_policy
    ADD CONSTRAINT recording_encode_policy_recording_id_fkey
    FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

-- goose_db_version がマイグレーションの版を持つため、重複していた目印を廃止する。
DROP TABLE public.schema_info;

CREATE INDEX recordings_rule_id_idx
    ON public.recordings (rule_id)
    WHERE rule_id IS NOT NULL;

-- +goose Down

DROP INDEX public.recordings_rule_id_idx;

ALTER TABLE public.recording_encode_policy
    DROP CONSTRAINT recording_encode_policy_recording_id_fkey;

ALTER TABLE public.recording_encode_policy
    ADD CONSTRAINT recording_encode_policy_recording_id_fkey
    FOREIGN KEY (recording_id) REFERENCES public.recordings(id);

CREATE TABLE public.schema_info (
    key text NOT NULL,
    value text NOT NULL,
    CONSTRAINT schema_info_pkey PRIMARY KEY (key)
);

INSERT INTO public.schema_info (key, value) VALUES ('version', '1');
