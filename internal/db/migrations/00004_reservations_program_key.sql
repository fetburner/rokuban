-- +goose Up

ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_pkey;

ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_site_program_id_key;

ALTER TABLE public.reservations
    DROP COLUMN id;

ALTER TABLE public.reservations
    ADD PRIMARY KEY (site, program_id);

-- +goose Down

ALTER TABLE public.reservations
    DROP CONSTRAINT reservations_pkey;

ALTER TABLE public.reservations
    ADD COLUMN id bigint GENERATED ALWAYS AS IDENTITY;

ALTER TABLE public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (id);

ALTER TABLE public.reservations
    ADD CONSTRAINT reservations_site_program_id_key UNIQUE (site, program_id);
