package store

import (
	"database/sql"

	// The store's only SQLite driver. It is cgo-free: unmodified SQLite
	// compiled to Wasm and run through wazero, so the release binary stays a
	// static binary with no toolchain dependency. No optional extensions
	// (FTS5 etc.) are enabled; a product todo asks for one before any lands.
	// Every other package reaches SQLite through this package, never by
	// importing the driver directly.
	_ "github.com/ncruces/go-sqlite3/driver"
)

// DriverName is the database/sql name the bundled driver registers. It is
// exported for diagnostics and tests; every real open goes through the
// forthcoming Open, so pool settings, paths and pragmas stay in one place.
// The DSN is a SQLite filename URI (for example "file:krowk.db"); Open builds
// it internally and never passes caller input through, because the driver
// honors full URIs (paths, modes, _pragma) and an open DSN would let a caller
// relocate the file or weaken durability behind Open's back.
const DriverName = "sqlite3"

// openSQL opens a database/sql handle using the bundled driver. It does not
// connect: the first statement does that, so callers Ping when they need the
// failure now rather than on first use.
func openSQL(dsn string) (*sql.DB, error) {
	return sql.Open(DriverName, dsn)
}
