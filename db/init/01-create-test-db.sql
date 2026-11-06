-- Runs ONCE, the first time the Postgres container creates its data volume
-- (files in /docker-entrypoint-initdb.d are executed by the official image).
--
-- It creates a separate database for integration tests, so running
-- `make test-integration` never deletes the seeded data in the main database.
CREATE DATABASE seatlock_test;
