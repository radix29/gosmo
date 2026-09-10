package gosmo

// This file keeps the "azuresql" driver from go-mssqldb/azuread registered
// with database/sql so external callers can still sql.Open it directly.
// gosmo's own connection path builds Entra credentials itself (entra.go) and
// never opens the registered driver by name; this blank import is what keeps
// the registration for everyone else.

import _ "github.com/microsoft/go-mssqldb/azuread"
