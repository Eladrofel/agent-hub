// Package db owns the embedded SQL migrations consumed by internal/store.
// Splitting this out keeps the SQL files at a sensible canonical location
// (gateway/db/migrations/) — close to the binary, mountable into Postgres's
// /docker-entrypoint-initdb.d, and readable from the source tree by operators
// running ad-hoc psql.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
