-- +goose Up

ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_pkey;

-- PostgreSQL の UNIQUE 制約は専用のインデックスを所有しているため、
-- そのまま PRIMARY KEY USING INDEX には渡せない。制約を外して同じキーの
-- ユニークインデックスを作り直し、複合主キーへ昇格させる。
ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_site_program_id_key;

ALTER TABLE public.reservations
    DROP COLUMN id;

CREATE UNIQUE INDEX reservations_site_program_id_key
    ON public.reservations (site, program_id);

ALTER TABLE public.reservations
    ADD PRIMARY KEY USING INDEX reservations_site_program_id_key;

-- +goose Down

ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_site_program_id_key;

ALTER TABLE public.reservations
    ADD COLUMN id bigint GENERATED ALWAYS AS IDENTITY;

ALTER TABLE public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (id);

ALTER TABLE public.reservations
    ADD CONSTRAINT reservations_site_program_id_key UNIQUE (site, program_id);
