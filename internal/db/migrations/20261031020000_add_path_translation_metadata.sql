-- +goose Up
CREATE TABLE path_translation_metadata (
    digest TEXT NOT NULL,
    policy TEXT NOT NULL,
    PRIMARY KEY (digest, policy)
);

-- +goose Down
DROP TABLE path_translation_metadata;
