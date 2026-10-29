-- +goose Up

-- awaiting_review_profiles は、cut プロファイルを凍結しているが、確認する前に
-- 状態が進む理由も無い録画×プロファイルを返す。件数の単位はプロファイルで、
-- API は録画 ID を重複排除し、メトリクスはプロファイル別に集計する。
CREATE FUNCTION awaiting_review_profiles(p_cut_profiles text[])
RETURNS TABLE(recording_id bigint, profile text)
    LANGUAGE sql STABLE
    AS $$
    SELECT u.recording_id, u.profile
    FROM unencoded_desired_profiles u
    JOIN encodable_recordings er ON er.recording_id = u.recording_id
    WHERE u.profile = ANY(p_cut_profiles)
      AND NOT EXISTS (
          SELECT 1 FROM recording_chapter_ownership o
          WHERE o.recording_id = u.recording_id
      )
      AND NOT EXISTS (
          SELECT 1 FROM recording_encode_attempts a
          WHERE a.recording_id = u.recording_id
            AND a.profile = u.profile
      )
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q
          WHERE q.recording_id = u.recording_id
            AND q.profile = u.profile
      )
$$;

COMMENT ON FUNCTION awaiting_review_profiles(text[]) IS
    'awaiting_review_profiles は、現在の cut 設定にある凍結済みプロファイルのうち、原本が active で章が未確認、active な encoded・試行行・未充足の作り直し要求が無い (recording_id, profile) を返す。';

-- +goose Down

DROP FUNCTION awaiting_review_profiles(text[]);
