-- name: CreateUser :one
-- Create a new user.
INSERT INTO
  users (
    email,
    id,
    name,
    roles,
    tz
  )
VALUES
  (
    $1,
    $2,
    $3,
    $4,
    $5
  )
RETURNING
  id,
  created_at;

-- name: GetUserByID :one
-- Get a user by their ID.
SELECT
  id,
  closed_at,
  created_at,
  disabled_at,
  email,
  name,
  roles,
  tz,
  updated_at,
  verified_at
FROM
  users
WHERE
  id = $1;