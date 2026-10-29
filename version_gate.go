package gosmo

// Version gating: naming a column the connected instance does not have fails
// the whole read, so a query that reads a column added in a later release has
// to ask the server's version first.

// serverMajorVersion is the instance's major version, or 0 when it was never
// read — a Server built without NewServer, which skips loadInfo — and also 0
// on an Azure engine edition, where the version that was read is not a feature
// level at all.
//
// Azure returns a frozen ProductVersion: a Managed Instance says 12.0.2000.8,
// SQL Server 2014, while running an 18.x engine that has every catalog column
// gosmo gates on 2016, 2017, 2019 and 2022 and creates databases at
// compatibility level 170. Believing that 12 puts an MI below every gate here,
// and the failure is silent — colSince substitutes a zero literal, so the read
// succeeds and simply returns nothing for columns the instance has. 0 routes
// it through the "never read ⇒ treat as newest" convention colSince documents,
// which is the right answer for Azure for the same reason: the query is a
// better authority than a version number that was never about features.
//
// info.VersionMajor keeps its 12 — callers that display a version want what
// the server says.
func (s *Server) serverMajorVersion() int {
	if s == nil {
		return 0
	}
	return s.info.featureMajor()
}

// serverMajorVersion is the Server rule for the database-scoped gates: 0 for a
// Database with no Server as well.
func (d *Database) serverMajorVersion() int {
	if d == nil || d.server == nil {
		return 0
	}
	return d.server.serverMajorVersion()
}

// featureMajor is VersionMajor as a feature level: 0 on a nil ServerInfo and
// on an Azure edition, for the reasons serverMajorVersion gives.
func (i *ServerInfo) featureMajor() int {
	if i == nil || i.IsAzure() {
		return 0
	}
	return i.VersionMajor
}

// majorAtLeast is the one version comparison in gosmo: major is at least v, or
// 0 — never read, or an Azure edition — which counts as newest. Every gate goes
// through it (or majorBefore, or the Server/Database forms below), never
// through a bare comparison: the 2026-10-21 review found the AG lag and
// seeding-mode reads gated with a bare `major < 13`, which blanked both columns
// on every Managed Instance. TestVersionGatesGoThroughMajorAtLeast enforces it.
func majorAtLeast(major int, v ServerVersion) bool {
	return major == 0 || major >= int(v)
}

// majorBefore is !majorAtLeast: major is known and older than v. It is the
// form a refusal ("needs SQL Server 2019 or later") reads naturally in.
func majorBefore(major int, v ServerVersion) bool {
	return !majorAtLeast(major, v)
}

// majorKnownAtLeast is the positive gate: major is known (not 0) and at least
// v. Only a choice where guessing "newest" fails hard while guessing "older"
// merely degrades uses it — EnumFileSystem's DMV-or-xp_dirtree pick. Every
// read and refusal gate uses majorAtLeast.
func majorKnownAtLeast(major int, v ServerVersion) bool {
	return major != 0 && major >= int(v)
}

// buildAtLeast is majorAtLeast for a feature that arrived in a service pack of
// an older major rather than with a major: true when the instance is a later
// major than v, or v itself at build or later. An unread version, an Azure
// edition and a nil ServerInfo count as newest, as majorAtLeast does.
//
// It is the one place a build number decides a gate. The floor rule (never
// gate at or below 2016) is about majors: gosmo supports 2016 from SP1
// (minimumServerBuild), so a feature SP2 added — sys.dm_db_log_info, 13.0.5026
// — is a gate inside the supported range, not below it.
func buildAtLeast(info *ServerInfo, v ServerVersion, build int) bool {
	major := info.featureMajor()
	if !majorAtLeast(major, v) {
		return false
	}
	return major != int(v) || info.VersionBuild >= build
}

// atLeast reports whether the connected instance is v or later, with an
// unread version and Azure counting as newest (majorAtLeast).
func (s *Server) atLeast(v ServerVersion) bool { return majorAtLeast(s.serverMajorVersion(), v) }

// before reports whether the connected instance is known to be older than v.
func (s *Server) before(v ServerVersion) bool { return majorBefore(s.serverMajorVersion(), v) }

// atLeast is Server.atLeast for a database-scoped gate.
func (d *Database) atLeast(v ServerVersion) bool { return majorAtLeast(d.serverMajorVersion(), v) }

// before is Server.before for a database-scoped gate.
func (d *Database) before(v ServerVersion) bool { return majorBefore(d.serverMajorVersion(), v) }

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
	if majorAtLeast(major, since) {
		return col
	}
	return zero
}

// bitSince is colSince for a bit column, whose zero is CAST(0 AS bit) — most
// gated columns are flags, and spelling the CAST at every site is how one of
// them would end up uncast.
func bitSince(major int, since ServerVersion, col string) string {
	return colSince(major, since, col, "CAST(0 AS bit)")
}

// hasColumnSince reports what colSince decides, for the rare gate that cannot
// be expressed as a substituted expression — a GROUP BY term, where SQL Server
// rejects a constant ("Each GROUP BY expression must contain at least one
// column that is not an outer reference") and the term has to be dropped
// outright rather than replaced.
//
// It answers exactly as majorAtLeast does; the separate name is what lets the
// gate inventory count GROUP BY gates.
func hasColumnSince(major int, since ServerVersion) bool {
	return majorAtLeast(major, since)
}
