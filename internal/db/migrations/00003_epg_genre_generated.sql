-- +goose Up

DROP INDEX public.epg_programs_genre_lv1_idx;

ALTER TABLE public.epg_programs
    DROP COLUMN genre_lv1;

ALTER TABLE public.epg_programs
    ADD COLUMN genre_lv1 smallint[]
        GENERATED ALWAYS AS (public.genre_lv1_of(genres)) STORED;

CREATE INDEX epg_programs_genre_lv1_idx
    ON public.epg_programs USING gin (genre_lv1);

-- +goose Down

DROP INDEX public.epg_programs_genre_lv1_idx;

ALTER TABLE public.epg_programs
    DROP COLUMN genre_lv1;

ALTER TABLE public.epg_programs
    ADD COLUMN genre_lv1 smallint[] NOT NULL DEFAULT '{}'::smallint[];

UPDATE public.epg_programs
SET genre_lv1 = public.genre_lv1_of(genres);

CREATE INDEX epg_programs_genre_lv1_idx
    ON public.epg_programs USING gin (genre_lv1);
