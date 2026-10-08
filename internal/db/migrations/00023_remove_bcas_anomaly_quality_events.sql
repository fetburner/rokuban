-- +goose Up

-- Scramble counts are already stored in drop_stats.scrambled. Remove their duplicate
-- quality event while preserving every other event and its order.
-- updated_at tracks changes to the stored recording row, including this cleanup.
UPDATE recordings AS r
SET quality_events = (
        SELECT COALESCE(jsonb_agg(event.value ORDER BY event.ordinality), '[]'::jsonb)
        FROM jsonb_array_elements(r.quality_events) WITH ORDINALITY AS event(value, ordinality)
        WHERE event.value->>'event' IS DISTINCT FROM 'bcas_anomaly'
    ),
    updated_at = now()
WHERE r.quality_events @> '[{"event":"bcas_anomaly"}]'::jsonb;

-- +goose Down

-- drop_stats.scrambled can show whether the anomaly happened, but not the original
-- application timestamp. Keep the cleaned history instead of fabricating an event time.
SELECT 1;
