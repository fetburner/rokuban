-- +goose Up

ALTER TABLE recording_encode_policy
    ADD COLUMN cm_detect_changed_at timestamptz;

CREATE FUNCTION public.cm_policy_next_generation_changed_at(previous_changed_at timestamptz)
RETURNS timestamptz
    LANGUAGE sql VOLATILE
AS $$
    SELECT GREATEST(
        clock_timestamp(),
        COALESCE(previous_changed_at + interval '1 microsecond', '-infinity'::timestamptz)
    )
$$;

CREATE FUNCTION public.cm_attempt_is_current_generation(
    attempt_at timestamptz,
    policy_changed_at timestamptz
) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT policy_changed_at IS NULL OR attempt_at > policy_changed_at
$$;

CREATE FUNCTION public.cm_attempt_is_prior_enabled_generation(
    target_recording_id bigint,
    attempt_at timestamptz
) RETURNS boolean
    LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT COALESCE((
        SELECT p.cm_detect
               AND NOT cm_attempt_is_current_generation(attempt_at, p.cm_detect_changed_at)
        FROM recording_encode_policy p
        WHERE p.recording_id = target_recording_id
    ), false)
$$;

-- Existing disabled policies may already have an attempt row. Treat the migration
-- as their last state change so a rapid re-enable before the first reconcile clears it.
UPDATE recording_encode_policy
SET cm_detect_changed_at = now()
WHERE NOT cm_detect;

-- +goose Down

DROP FUNCTION public.cm_attempt_is_prior_enabled_generation(bigint, timestamptz);
DROP FUNCTION public.cm_attempt_is_current_generation(timestamptz, timestamptz);
DROP FUNCTION public.cm_policy_next_generation_changed_at(timestamptz);

ALTER TABLE recording_encode_policy
    DROP COLUMN cm_detect_changed_at;
