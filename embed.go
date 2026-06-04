// Package nexus exposes embedded assets (SQL migrations) shared across binaries.
package nexus

import "embed"

// Migrations holds SQL migration files for Postgres and ClickHouse.
//
//go:embed migrations
var Migrations embed.FS
