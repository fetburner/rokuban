-- +goose Up

-- 00014 で足した recordings_series_key_idx を落とす。今の録画クエリは
-- recordings.series_key で絞らない。recording_series ビューは
-- COALESCE(value_key, series_key) を出し、棚はこの値でグループ化し、一覧の
-- シリーズ絞り込みもビューの値で比べる。分析済みの録画 20,000 件で
-- `SELECT * FROM recording_series WHERE value = '作品1'` の EXPLAIN を見ると
-- recordings（と label_rule_hits）は Seq Scan のままで、ビューを recording_id で
-- 結合しても recordings は Seq Scan になる。この索引には今の読み手がいない。
--
-- epg_programs_series_key_idx は残す。epg_program_series の自動キー側が
-- epg_programs.series_key を直接出しているからである。ビューの絞り込みは
-- この索引を使った（ローカルの 20,000 行では Index Scan。走査の種類は
-- データの分布で変わる）。
-- 後で recordings.series_key を使える録画クエリを足すときは、そのクエリと同じ
-- 変更で索引を足す。
--
-- 00014 の series_key 作り直し手順のコメントにある recordings 側の CREATE INDEX は
-- もう要らない（この索引は張り直さない）。
DROP INDEX public.recordings_series_key_idx;

-- +goose Down

CREATE INDEX recordings_series_key_idx ON public.recordings (series_key);
