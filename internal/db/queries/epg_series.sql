-- name: ListUpcomingProgramsBySeries :many
-- ハブ（番組ハブ）の「次回」。起点の録画の実効シリーズと同じ実効シリーズを持つ、
-- これから放送される番組を返す。
--
--   起点の値が NULL（自動キーを導出できず、どのルールも当たらない録画）なら
--   0 件になる --- 実効シリーズが NULL の番組は `= NULL` に当たらない。
--
-- 返す形は検索（ProgramSearchMatch）と同じで、site を運び畳まない。予約状態は
-- 結合しない（番組と予約はキャッシュの寿命が違う。docs/api/rest.md「予約状態は
-- 番組と結合しない」）。
--
-- **実効シリーズの定義はビュー 2 つに閉じる**（recording_series /
-- epg_program_series）。COALESCE(lr.value_key, p.series_key) を書き下すと、
-- 録画側（棚・一覧）と EPG 側（ハブの次回）で規則が 2 箇所に分かれる。
--
-- start_at > now() の窓は EPG のローリングウィンドウ（mirakc の保持期間）が
-- そのまま上限になる。ページネーションは持たない --- 同じ棚の未来の回は
-- 数十件で、有界である。
SELECT p.site,
       p.program_id,
       p.network_id,
       p.service_id,
       p.start_at,
       p.duration_ms,
       p.name,
       p.is_free
FROM epg_program_series s
JOIN epg_programs p ON p.site = s.site AND p.program_id = s.program_id
WHERE s.value = (
        SELECT rs.value FROM recording_series rs
        WHERE rs.recording_id = sqlc.arg(recording_id)::bigint
      )
  AND p.start_at > now()
ORDER BY p.start_at ASC, p.site ASC, p.program_id ASC;
