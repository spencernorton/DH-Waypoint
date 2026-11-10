-- +goose NO TRANSACTION
-- +goose Up
-- Migration 00012 built a GIN index over (queue, payload). No query filters
-- queue_jobs with a jsonb containment or existence operator, so the planner
-- never reads it, yet every job insert and status change pays to maintain it
-- and it grows far larger than the table. Dropping it changes no query result.
DROP INDEX CONCURRENTLY IF EXISTS queue_jobs_queue_payload_idx;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS queue_jobs_queue_payload_idx
    ON queue_jobs USING gin (queue, payload);
