-- +goose Up
-- CM 検出の局単位の状態（ロゴの枠・候補・学習済みロゴ）。列と CHECK は Postgres の最終形（internal/db/schema.sql）に揃える。
-- 時刻は DATETIME(6)、バイト列は LONGBLOB。Postgres の通知トリガーは持たないので、この表にはトリガーを張らない。

CREATE TABLE cm_logo_areas (
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    x INT NOT NULL,
    y INT NOT NULL,
    w INT NOT NULL,
    h INT NOT NULL,
    coded_width INT NOT NULL,
    coded_height INT NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (network_id, service_id),
    CHECK (coded_width > 0 AND coded_height > 0 AND x >= 0 AND y >= 0 AND w > 0 AND h > 0
           AND x + w <= coded_width AND y + h <= coded_height)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE cm_logos (
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    lgd LONGBLOB NOT NULL,
    preview_png LONGBLOB,
    learned_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    learned_from BIGINT,
    coded_width INT NOT NULL,
    coded_height INT NOT NULL,
    PRIMARY KEY (network_id, service_id),
    FOREIGN KEY (learned_from) REFERENCES recordings(id),
    CHECK (coded_width > 0 AND coded_height > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 枠を消すと候補も消える（Postgres の ON DELETE CASCADE と同じ）。FK の CASCADE はトリガーを発火させない。
CREATE TABLE cm_logo_candidates (
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    state VARCHAR(8) NOT NULL CHECK (state IN ('running', 'failed', 'ready')),
    stage VARCHAR(10) CHECK (stage IS NULL OR stage IN ('setup', 'probe', 'area', 'logo', 'match', 'save', 'stopped')),
    error LONGTEXT,
    x INT NOT NULL,
    y INT NOT NULL,
    w INT NOT NULL,
    h INT NOT NULL,
    coded_width INT NOT NULL,
    coded_height INT NOT NULL,
    recording_id BIGINT,
    observed_area_updated_at DATETIME(6) NOT NULL,
    lgd LONGBLOB,
    preview_png LONGBLOB,
    attempted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (network_id, service_id),
    FOREIGN KEY (network_id, service_id) REFERENCES cm_logo_areas(network_id, service_id) ON DELETE CASCADE,
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK (coded_width > 0 AND coded_height > 0 AND x >= 0 AND y >= 0 AND w > 0 AND h > 0
           AND x + w <= coded_width AND y + h <= coded_height)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- CM 検出が未了で再試行の対象になる録画（Postgres の cm_detection_desired と同じ述語）。
CREATE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_processing_policy p ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c WHERE c.media_asset_id = o.id)
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND (ca.recording_id IS NULL OR ca.state <> 'failed'
       OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at));

-- ロゴ候補の解析が必要な局（Postgres の cm_logo_candidate_desired と同じ述語）。
-- recording_id は解析に使う録画（実体のある原本を持つ最新）。無ければ NULL。
CREATE VIEW cm_logo_candidate_desired AS
SELECT area.network_id, area.service_id, area.updated_at AS area_updated_at,
       (SELECT r2.id
        FROM recordings r2
        JOIN media_assets original ON original.recording_id = r2.id AND original.kind = 'original'
        WHERE r2.network_id = area.network_id AND r2.service_id = area.service_id
          AND r2.deleted_at IS NULL AND r2.purged_at IS NULL
          AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c WHERE c.media_asset_id = original.id)
        ORDER BY r2.id DESC
        LIMIT 1) AS recording_id
FROM cm_logo_areas area
WHERE NOT EXISTS (SELECT 1 FROM cm_logo_candidates candidate
                  WHERE candidate.network_id = area.network_id AND candidate.service_id = area.service_id)
  AND NOT EXISTS (SELECT 1 FROM cm_logos logo
                  WHERE logo.network_id = area.network_id AND logo.service_id = area.service_id
                    AND logo.learned_at >= area.updated_at)
  AND EXISTS (SELECT 1
              FROM recordings r
              JOIN media_assets original ON original.recording_id = r.id AND original.kind = 'original'
              WHERE r.network_id = area.network_id AND r.service_id = area.service_id
                AND r.deleted_at IS NULL AND r.purged_at IS NULL
                AND EXISTS (SELECT 1 FROM active_present_media_asset_copies c WHERE c.media_asset_id = original.id));

-- +goose Down
DROP VIEW cm_logo_candidate_desired;
DROP VIEW cm_detection_desired;
DROP TABLE cm_logo_candidates;
DROP TABLE cm_logos;
DROP TABLE cm_logo_areas;
