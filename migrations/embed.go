package migrations

import "embed"

// The broad pattern lets this package compile before the first migration exists;
// goose ignores every file not named NNNNN_name.sql.
//
//go:embed *
var FS embed.FS
