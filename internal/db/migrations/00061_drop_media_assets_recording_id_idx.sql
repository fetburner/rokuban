-- +goose Up

-- recording_id で media_assets を引く読み手は、一意制約
-- media_assets_recording_id_kind_profile_key（recording_id, kind, profile）の
-- 先頭列で引ける。本番計測では、この索引を選んでいた読み手はどれも kind を
-- Filter で当てていた（kind = 'original' の等値か、kind の OR）。落とした後の
-- 計画では、一意制約の索引が kind を Index Cond に含めて引いた。範囲条件
-- （recording_id > $n）で引く読み手も同じだった。計画が変わらない読み手には
-- 影響しない。重複した索引は書き込みのたびに更新されるため削除する。
DROP INDEX public.media_assets_recording_id_idx;

-- +goose Down

CREATE INDEX media_assets_recording_id_idx ON public.media_assets (recording_id);
