-- +goose Up
ALTER TABLE workers ADD COLUMN instance_id uuid;
CREATE UNIQUE INDEX workers_instance_id_idx ON workers (instance_id);

-- +goose Down
DROP INDEX workers_instance_id_idx;
ALTER TABLE workers DROP COLUMN instance_id;
