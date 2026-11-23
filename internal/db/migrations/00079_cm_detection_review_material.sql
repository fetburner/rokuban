-- +goose Up

-- Review material has the same detection-row lifetime and commit point as cm_ranges.
-- Paired NULL values mark pre-migration rows whose review material is unavailable.
ALTER TABLE recording_cm_detections
    ADD COLUMN logo_ranges int8multirange,
    ADD COLUMN structure_elements jsonb,
    ADD CONSTRAINT recording_cm_detections_review_material_pair
        CHECK ((logo_ranges IS NULL) = (structure_elements IS NULL));

-- +goose Down

ALTER TABLE recording_cm_detections
    DROP CONSTRAINT recording_cm_detections_review_material_pair,
    DROP COLUMN structure_elements,
    DROP COLUMN logo_ranges;
