-- +goose Up

-- 人が教えたロゴの枠。行があること = 教えた枠（自動で推定した枠は保存しない）。
--
-- **自動推定の枠と同じ列に置かない**（不変条件 9）。logoframe は自動推定に
-- 失敗すると何も書かないが、成功した枠を後で自動側が「外したら忘れる」処理で
-- 上書きすると、人の入力が消える。
--
-- 座標は記録上の解像度（coded_width / coded_height。地上波 HD の 1440x1080 など）。
-- poster やシークタイルの座標は使えない --- あちらは SAR を正方形画素へ
-- 焼き込んでいるので、1440x1080 の映像では x が 4/3 倍ずれる。
CREATE TABLE cm_logo_areas (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    x integer NOT NULL,
    y integer NOT NULL,
    w integer NOT NULL,
    h integer NOT NULL,
    coded_width integer NOT NULL,
    coded_height integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (network_id, service_id),
    -- logoframe は枠が映像の外にあると失敗する（-logo-area は x+w <= width を
    -- 要求する）。表現不可能にしておく（不変条件 10）。
    CONSTRAINT cm_logo_areas_rect_within_frame CHECK (
        coded_width > 0
        AND coded_height > 0
        AND x >= 0
        AND y >= 0
        AND w > 0
        AND h > 0
        AND x + w <= coded_width
        AND y + h <= coded_height
    )
);

-- +goose Down

DROP TABLE cm_logo_areas;
