package gosmo

// This file registers the "azuresql" driver from go-mssqldb/azuread so that
// Entra ID authentication methods work without any WMI or COM dependencies.
// The import is side-effect only; no exported symbols are used directly.

import _ "github.com/microsoft/go-mssqldb/azuread"
