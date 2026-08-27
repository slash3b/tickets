package store

import _ "embed"

// SchemaSQL is exported so an integration test can build the whole system
// against one database. Production applies migrations, not this.
//
//go:embed schema.sql
var SchemaSQL string
