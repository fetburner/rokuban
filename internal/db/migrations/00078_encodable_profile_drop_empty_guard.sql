-- +goose Up

-- encodable_profile の先頭の `profile_name <> ''` を外す。空の名前は known_profiles に
-- 入らないので、`= ANY(known_profiles)` が同じ判定をする（internal/config の
-- TestLoad_EncodeProfileValidation/empty_name が、空名を設定で拒むことを固定する）。
-- known_profiles は encode の再照合（internal/encode/encode_reconcile.go）と api（cmd/rokuban/server.go）の
-- どちらも ProfileNames() から得る。
-- 条件が効くのは known_profiles に '' が入るときだけで、それは到達しない。残すと、空名と
-- NULL の組み合わせだけ FALSE を返し、ほかの名前の NULL（COMMENT どおり NULL）と食い違う。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[])
RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT profile_name = ANY(known_profiles)
       AND (
           NOT (profile_name = ANY(cut_profiles))
           OR EXISTS (
               SELECT 1 FROM recording_chapter_ownership o
               WHERE o.recording_id = rec_id
           )
       )
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[])
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
-- +goose StatementEnd
