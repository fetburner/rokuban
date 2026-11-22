-- +goose Up
CREATE TABLE circuit_breakers (
    scope VARCHAR(255) NOT NULL,
    name VARCHAR(255) NOT NULL,
    tripped_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    pending INT NOT NULL,
    threshold INT NOT NULL,
    detail JSON NOT NULL,
    PRIMARY KEY (scope, name)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE circuit_breakers;
