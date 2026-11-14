-- +goose Up

-- recording_id で media_assets を引く読み手は、一意制約
-- media_assets_recording_id_kind_profile_key（recording_id, kind, profile）の
-- 先頭列で引ける。本番計測では、kind = 'original' の等値で絞る文は、削除後に
-- 一意制約の索引を recording_id と kind の Index Cond で引いた。範囲条件
-- （recording_id > $n）で引く文も、削除後に recording_id と kind を
-- Index Cond に含めて一意制約の索引を引いた。一方、
-- removed_encoded_assets view の other を kind の OR で絞る文は、削除後も
-- recording_id だけを Index Cond に置き、選ぶ索引だけが削除前の
-- media_assets_recording_id_idx から削除後の一意制約の索引に変わった。
-- 計画が変わらない読み手には影響しない。重複した索引は INSERT のたびに
-- 更新されるため削除する。
DROP INDEX public.media_assets_recording_id_idx;

-- +goose Down

CREATE INDEX media_assets_recording_id_idx ON public.media_assets (recording_id);
