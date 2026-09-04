package gosmo

// Version gating: naming a column the connected instance does not have fails
// the whole read, so a query that reads a column added in a later release has
// to ask the server's version first.

// serverMajorVersion is the instance's major version, or 0 when it was never
// read — a Server built without NewServer, which skips loadInfo.
func (s *Server) serverMajorVersion() int {
	if s == nil || s.info == nil {
		return 0
	}
	return s.info.VersionMajor
}

// colSince names col when major is at least since, and otherwise substitutes
// zero, a typed literal of col's type. The scan destination list is unchanged
// either way, so a caller gets a zero value on an older instance rather than
// an "invalid column name" that kills the read — and a Scan arity error, the
// failure mode of omitting the column instead, cannot happen.
//
// major of 0 means the version was never read; treat that as newest, the
// convention every other version gate here follows, so a Server built without
// NewServer is not silently degraded.
//
// zero must be a CAST to col's own type. Without the CAST the driver reports a
// column of whatever type the literal parsed as, and a destination expecting a
// bit or a bigint fails to scan it.
func colSince(major int, since ServerVersion, col, zero string) string {
	if major != 0 && major < int(since) {
		return zero
	}
	return col
}

// hasColumnSince reports what colSince decides, for the rare gate that cannot
// be expressed as a substituted expression — a GROUP BY term, where SQL Server
// rejects a constant ("Each GROUP BY expression must contain at least one
// column that is not an outer reference") and the term has to be dropped
// outright rather than replaced.
func hasColumnSince(major int, since ServerVersion) bool {
	return major == 0 || major >= int(since)
}
