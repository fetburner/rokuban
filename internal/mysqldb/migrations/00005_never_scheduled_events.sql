-- +goose Up
CREATE TABLE never_scheduled_events (
    site VARCHAR(255) NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    event_id INT NOT NULL,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    program_start_at DATETIME(6) NOT NULL,
    PRIMARY KEY (site, network_id, service_id, event_id, program_start_at)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE never_scheduled_events;
