-- +goose Up

-- 00014 added recordings_series_key_idx, but current recording queries do not
-- filter on recordings.series_key. recording_series exposes COALESCE(value_key,
-- series_key); recording shelves group by that value, and recording-list series
-- filters also compare that view value. With 20,000 analyzed recordings,
-- EXPLAIN for `SELECT * FROM recording_series WHERE value = '作品1'` still shows
-- Seq Scan on recordings (and label_rule_hits), while joining the view by
-- recording_id also shows Seq Scan on recordings. The index therefore has no
-- current reader.
--
-- Keep epg_programs_series_key_idx: the automatic-key arm of
-- epg_program_series exposes epg_programs.series_key directly. EXPLAIN for
-- filtering that view uses epg_programs_series_key_idx (Index Scan in the
-- 20,000-row local run; the scan type depends on the data distribution).
-- If a recording query that can use recordings.series_key is added later, add
-- its index in the same change as that query.
DROP INDEX public.recordings_series_key_idx;

-- +goose Down

CREATE INDEX recordings_series_key_idx ON public.recordings (series_key);
