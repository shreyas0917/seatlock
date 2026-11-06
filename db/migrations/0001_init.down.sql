-- Reverses 0001_init. Tables are dropped in reverse dependency order:
-- bookings references seats, and seats references shows.
DROP TABLE IF EXISTS bookings;
DROP TABLE IF EXISTS seats;
DROP TABLE IF EXISTS shows;
