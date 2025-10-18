CREATE TABLE users (
  id UUID NOT NULL PRIMARY KEY, created_at timestamp NOT NULL DEFAULT NOW(),
  email VARCHAR(512) NOT NULL UNIQUE CONSTRAINT email_has_at_and_dot CHECK (
   email LIKE '%_@_%._%' -- simple email validation: must have at least three non-empty parts separated by @ and . --
   ),
  name VARCHAR(256) NOT NULL,  -- displayed username: todo: constraints: min length, max length, valid chars--
  roles USER_ROLE [] NOT NULL DEFAULT '{TRADER}',
  -- updates
  updated_at timestamp NOT NULL DEFAULT NOW() CONSTRAINT updated_at_gte_created_at CHECK (updated_at >= created_at),
  disabled_at timestamp NULL CONSTRAINT disabled_at_null_or_gte_created_at CHECK (
    (disabled_at IS NULL)
    OR disabled_at >= created_at
  ),
  tz varchar(256) NOT NULL DEFAULT 'UTC',
  closed_at timestamp NULL,  -- if not null, this account was closed at this time --
  verified_at timestamp NULL  -- if not null, this account was verified at this time --
);