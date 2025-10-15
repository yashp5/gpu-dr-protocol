#!/usr/bin/env bash

set -eu

printf 'lint.bash: run go linters to check code quality

    shfmt: format shell scripts
    gofumpt: stricter gofmt + goimports
    staticcheck: static analysis (linting)
    go vet: static analysis (linting)
    sqlc vet: static analysis of postgres queries and database schema
    sqlc compile: check that postgres queries and database schema compile
'
export POSTGRES_USER=postgres

(go tool shfmt -s -w . && echo "shfmt: OK") || (echo "shfmt: failed" && exit 1) &
(go tool gofumpt -w . && echo "gofumpt: OK") || echo "gofumpt: failed" && exit 1 &
(go tool staticcheck ./... && echo "staticcheck: OK") || (echo "staticcheck: failed" && exit 1) &
(go vet ./... && echo "go vet: OK") || (echo "go vet: failed" && exit 1) &
(go tool sqlc vet && echo "sqlc vet: OK") || (echo "sqlc vet: failed: failed: no local postgres?") &
(go tool sqlc compile && echo "sqlc compile: OK") || (echo "sqlc compile: failed: no local postgres?") &

HELP='
HELP:
if you get an error message like:
    managed: create database: failed to connect to user=efron database=postgres`: 127.0.0.1:5432 (localhost): dial error: dial tcp 127.0.0.1:5432: connect: connection refused
try:
    "make postgres" to start a local postgres instance
'

wait || echo "$HELP"
