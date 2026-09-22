package gosmo

// capabilities_scan.go reads what capabilities_query.go asked: one row per
// name per scope, into the maps Capabilities and DatabaseCapabilities answer
// from. A NULL answer is CapabilityUnknown, never a denial — see
// CapabilityState.

import (
	"database/sql"
	"strings"
)

// capabilityDest is the set of maps scanCapabilityRows fills, one per block the
// probe can send.
//
// It is a struct rather than eight parameters because the server probe shares
// the scanner and asks for two of the blocks: it passed six nils in a row, and
// a seventh added in the wrong position would have been silently accepted. A
// nil field still means "this probe did not ask for that block", and its rows
// are dropped.
type capabilityDest struct {
	roles              map[string]bool
	perms              map[string]CapabilityState
	explicitServer     map[string]map[string]CapabilityState
	availabilityGroups map[string]map[string]CapabilityState
	schemas            map[string]map[string]CapabilityState
	explicitSchemas    map[string]map[string]CapabilityState
	explicitDB         map[string]CapabilityState
	explicitPrincipals map[string]map[string]CapabilityState
	objects            map[string]map[string]CapabilityState
	columns            map[string]map[string]CapabilityState
	securables         map[string]map[string]CapabilityState
}

// scanCapabilityRows fills into from the probe's (kind, name, answer) rows. A
// NULL answer is left out of the map entirely, which is what makes it read back
// as CapabilityUnknown / false.
func scanCapabilityRows(rows *sql.Rows, into capabilityDest) error {
	for rows.Next() {
		var kind, name string
		var answer sql.NullInt64
		if err := rows.Scan(&kind, &name, &answer); err != nil {
			return err
		}
		switch {
		case kind == "R":
			if into.roles != nil {
				into.roles[name] = answer.Valid && answer.Int64 == 1
			}
		case kind == "P":
			if st, ok := capabilityStateOf(answer); ok && into.perms != nil {
				into.perms[name] = st
			}
		case strings.HasPrefix(kind, "S:"):
			// name is the schema here, and the permission rides in kind — see
			// schemaCapabilityQuery. A server probe passes a nil map and drops
			// these rows, which it never asks for.
			st, ok := capabilityStateOf(answer)
			if !ok || into.schemas == nil {
				continue
			}
			if into.schemas[name] == nil {
				into.schemas[name] = map[string]CapabilityState{}
			}
			into.schemas[name][strings.TrimPrefix(kind, "S:")] = st
		case strings.HasPrefix(kind, "E:"):
			// The schema catalog block, keyed by schema name. Kept apart from
			// the "S:" rows because those answer HAS_PERMS_BY_NAME, whose 0
			// cannot tell a DENY from a permission never granted — see
			// DatabaseCapabilities.ExplicitSchemaPermissions.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitSchemas, name, strings.TrimPrefix(kind, "E:"), st)
			}
		case strings.HasPrefix(kind, "D:"):
			// The database catalog block. name is DB_NAME() and is not read —
			// there is one database per probe — while the permission rides in
			// kind, as in every other catalog block. A server probe passes a
			// nil map and drops these rows, which it never asks for. Only DENY
			// rows are selected, so the state is always CapabilityDenied; it
			// is read back through capabilityStateOf anyway so a query change
			// that starts selecting grants is recorded rather than mislabelled.
			st, ok := capabilityStateOf(answer)
			if !ok || into.explicitDB == nil {
				continue
			}
			// A denial wins however the rows are ordered — recordSecurableState's
			// reasoning, one map shallower: the block can produce a row through
			// the login and another through a role it is in.
			perm := strings.TrimPrefix(kind, "D:")
			if into.explicitDB[perm] != CapabilityDenied {
				into.explicitDB[perm] = st
			}
		case strings.HasPrefix(kind, "N:"):
			// The class-4 catalog block, keyed by the principal's name. It is
			// "N:" rather than "P:" because "P" alone already tags the
			// HAS_PERMS_BY_NAME database-scope rows. Only DENY rows are
			// selected — see DatabaseCapabilities.ExplicitPrincipalPermissions
			// — and they are read back through capabilityStateOf anyway so a
			// query change that starts selecting grants is recorded rather
			// than mislabelled.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitPrincipals, name, strings.TrimPrefix(kind, "N:"), st)
			}
		case strings.HasPrefix(kind, "G:"):
			// The availability-group block, keyed by the group's name, with
			// the permission in kind as in every other per-securable block. It
			// is a HAS_PERMS_BY_NAME answer rather than a catalog row, so it
			// goes through capabilityStateOf for the same reason the "P" rows
			// do: a NULL is "this instance does not define the permission",
			// not a denial.
			st, ok := capabilityStateOf(answer)
			if !ok || into.availabilityGroups == nil {
				continue
			}
			if into.availabilityGroups[name] == nil {
				into.availabilityGroups[name] = map[string]CapabilityState{}
			}
			into.availabilityGroups[name][strings.TrimPrefix(kind, "G:")] = st
		case strings.HasPrefix(kind, "K:"):
			// The class 5/6/10/24/25/26 block, keyed by DatabaseSecurableKey. A
			// HAS_PERMS_BY_NAME answer like "G:", so a NULL is skipped rather
			// than recorded as a denial — and like "G:" there is one row per
			// securable, so nothing needs a denial to win over a grant.
			st, ok := capabilityStateOf(answer)
			if !ok || into.securables == nil {
				continue
			}
			if into.securables[name] == nil {
				into.securables[name] = map[string]CapabilityState{}
			}
			into.securables[name][strings.TrimPrefix(kind, "K:")] = st
		case strings.HasPrefix(kind, "V:"):
			// The server-scope catalog block, keyed "<kind>::<name>" — see
			// ServerSecurableKey. It is the one catalog block the *server*
			// probe asks for and the database probe does not, so the database
			// probe passes a nil map and drops these rows. Only DENY rows are
			// selected — see Capabilities.ExplicitServerPermissions — and they
			// are read back through capabilityStateOf anyway so a query change
			// that starts selecting grants is recorded rather than mislabelled.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitServer, name, strings.TrimPrefix(kind, "V:"), st)
			}
		case strings.HasPrefix(kind, "O:"):
			// As with "S:", name is the securable and the permission rides in
			// kind.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.objects, name, strings.TrimPrefix(kind, "O:"), st)
			}
		case strings.HasPrefix(kind, "C:"):
			// The column block, keyed "schema.object.column". It is kept apart
			// from the object map because a column row answers for the column
			// alone — see DatabaseCapabilities.ColumnPermissions.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.columns, name, strings.TrimPrefix(kind, "C:"), st)
			}
		}
	}
	return rows.Err()
}

// recordSecurableState files one object- or column-scope answer under its
// securable, into a map a server probe passes as nil and so drops.
//
// A denial wins over a grant however the two rows are ordered: SQL Server
// resolves DENY over GRANT, and the catalog block can produce both rows for
// one securable — a grant to a role the login is in, a deny to the login
// itself.
func recordSecurableState(m map[string]map[string]CapabilityState, key, perm string, st CapabilityState) {
	if m == nil {
		return
	}
	if m[key] == nil {
		m[key] = map[string]CapabilityState{}
	}
	if m[key][perm] == CapabilityDenied {
		return
	}
	m[key][perm] = st
}

// capabilityStateOf maps one HAS_PERMS_BY_NAME answer to a state. NULL is not
// a state: the permission does not apply to the securable on this instance,
// and recording it as denied would withhold whatever it gates.
func capabilityStateOf(answer sql.NullInt64) (CapabilityState, bool) {
	if !answer.Valid {
		return CapabilityUnknown, false
	}
	if answer.Int64 == 1 {
		return CapabilityGranted, true
	}
	return CapabilityDenied, true
}
