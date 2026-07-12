package gosmo

// This file keeps the "azuresql" driver from go-mssqldb/azuread registered
// with database/sql so external callers can still sql.Open it directly.
// gosmo's own connection path uses the azuread connector API (see
// buildConnector) rather than the registered driver name, but the blank
// import here preserves the registration side effect regardless.

import _ "github.com/microsoft/go-mssqldb/azuread"
