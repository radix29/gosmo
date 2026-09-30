package gosmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// resource_governor_write.go is the write half of resource_governor.go:
// create/alter/drop for resource pools, workload groups and external
// resource pools, and the ALTER RESOURCE GOVERNOR statements on the
// singleton.
//
// # Nothing here reconfigures
//
// Every write changes the stored configuration only and leaves the change
// pending until ALTER RESOURCE GOVERNOR RECONFIGURE — ResourceGovernor's
// Reconfigure. No method issues it on the caller's behalf: an editor applying
// a pool and three groups reconfigures once, and RECONFIGURE is not a neutral
// step, because it **enables a disabled governor** (probed 2026-09-30). A
// caller that wants the governor to stay disabled issues Disable afterwards,
// which clears the pending flag without applying anything.
//
// Pool and group DDL is transactional; RECONFIGURE is refused inside a user
// transaction (Msg 574). So a batch of these writes can be wrapped in one
// transaction, and the RECONFIGURE (or DISABLE) must follow the COMMIT.
//
// # What the server refuses, and gosmo lets through
//
// Probed on 17, and not pre-validated here — the server's message is the
// right one to show:
//
//   - ALTER of the internal pool or group: Msg 10915.
//   - DROP of a built-in pool, group or external pool: Msg 10912.
//   - DROP of a pool that still has workload groups: Msg 10916, naming one.
//   - DROP FUNCTION on the function that is the classifier: Msg 10920 —
//     SetClassifier with an empty name first.
//   - Out-of-range and MIN > MAX percentages.
//
// What gosmo does refuse is syntax an older server cannot parse: a
// fractional REQUEST_MAX_MEMORY_GRANT_PERCENT before SQL Server 2019 (a
// syntax error on 13.0.6500.1) and the tempdb-governance options before 2025.
// Both come back as ErrUnsupportedVersion before any statement is sent.
//
// # Statement shapes
//
//   - ALTER RESOURCE POOL / WORKLOAD GROUP / EXTERNAL RESOURCE POOL with no
//     WITH clause, or an empty WITH (), is a syntax error, so an Alter with
//     nothing set issues nothing.
//   - ALTER RESOURCE GOVERNOR WITH takes exactly one option per statement:
//     WITH (CLASSIFIER_FUNCTION = NULL, MAX_OUTSTANDING_IO_PER_VOLUME = 60)
//     is Msg 102. Hence one method per option.
//   - MAX_OUTSTANDING_IO_PER_VOLUME = 0 is refused (Msg 1040); DEFAULT is
//     how the stored value goes back to 0, "server chooses".
//   - The classifier's two-part name resolves in master whatever the
//     connection's current database (probed from tempdb), so no USE is
//     needed.
//   - None of the three DROPs has an IF EXISTS form (Msg 156).

// WorkloadImportance is a workload group's IMPORTANCE — the relative weight
// its requests get within the pool.
type WorkloadImportance string

const (
	ImportanceLow    WorkloadImportance = "Low"
	ImportanceMedium WorkloadImportance = "Medium" // the default
	ImportanceHigh   WorkloadImportance = "High"
)

// rgOptions accumulates the "NAME = value" items of one WITH (...) list.
type rgOptions struct{ parts []string }

func (o *rgOptions) int(name string, v *int) {
	if v != nil {
		o.parts = append(o.parts, fmt.Sprintf("%s = %d", name, *v))
	}
}

func (o *rgOptions) float(name string, v *float64) {
	if v != nil {
		o.parts = append(o.parts, name+" = "+formatRGNumber(*v))
	}
}

func (o *rgOptions) raw(item string) { o.parts = append(o.parts, item) }

// with renders " WITH (a, b)", or "" when nothing was set.
func (o *rgOptions) with() string {
	if len(o.parts) == 0 {
		return ""
	}
	return " WITH (" + strings.Join(o.parts, ", ") + ")"
}

// formatRGNumber renders a percentage or size the way T-SQL takes it: the
// shortest exact decimal, with no exponent and no trailing ".0" on a whole
// number.
func formatRGNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// -- Resource governor -----------------------------------------------------------

// ResourceGovernorRef returns a handle for the server's Resource Governor
// configuration without reading it. Every field is at its zero value;
// Server.ResourceGovernor is what populates them. It is the form to write
// through when there is nothing to read — under Scripting(ctx), or for a
// login that may alter the governor (CONTROL SERVER) but whose catalog read
// would still be the one gosmo reports as not found.
func (s *Server) ResourceGovernorRef() *ResourceGovernor {
	return &ResourceGovernor{server: s}
}

func (rg *ResourceGovernor) alter(ctx context.Context, clause, what string) error {
	if err := rg.server.exec(ctx, "ALTER RESOURCE GOVERNOR "+clause); err != nil {
		return fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return nil
}

// Reconfigure applies every stored change (ALTER RESOURCE GOVERNOR
// RECONFIGURE). It also enables the governor if it was disabled — there is
// no RECONFIGURE that leaves it off; follow it with Disable for that.
func (rg *ResourceGovernor) Reconfigure(ctx context.Context) error {
	if err := rg.alter(ctx, "RECONFIGURE", "reconfigure resource governor"); err != nil {
		return err
	}
	setIfApplied(ctx, &rg.IsEnabled, true)
	return nil
}

// Enable enables the governor. It is the same statement as Reconfigure —
// SQL Server has no separate ENABLE — so it applies every pending change too.
func (rg *ResourceGovernor) Enable(ctx context.Context) error {
	if err := rg.alter(ctx, "RECONFIGURE", "enable resource governor"); err != nil {
		return err
	}
	setIfApplied(ctx, &rg.IsEnabled, true)
	return nil
}

// Disable disables the governor (ALTER RESOURCE GOVERNOR DISABLE). It also
// clears the pending flag without applying the stored changes; the next
// Reconfigure applies them.
func (rg *ResourceGovernor) Disable(ctx context.Context) error {
	if err := rg.alter(ctx, "DISABLE", "disable resource governor"); err != nil {
		return err
	}
	setIfApplied(ctx, &rg.IsEnabled, false)
	return nil
}

// ResetStatistics restarts the cumulative counters ResourcePoolStats and
// WorkloadGroupStats report (ALTER RESOURCE GOVERNOR RESET STATISTICS).
func (rg *ResourceGovernor) ResetStatistics(ctx context.Context) error {
	return rg.alter(ctx, "RESET STATISTICS", "reset resource governor statistics")
}

// SetClassifier makes schema.name in master the classifier function, or
// removes the classifier when name is empty. The function must already exist
// in master, be schema-bound and return sysname; the server enforces that.
// The change is pending until Reconfigure.
//
// ClassifierSchema and ClassifierName are mirrored onto the receiver;
// ClassifierFunctionID is zeroed on removal and otherwise left as it was —
// the new object_id is not read back.
func (rg *ResourceGovernor) SetClassifier(ctx context.Context, schema, name string) error {
	fn := "NULL"
	if name != "" {
		if schema == "" {
			return fmt.Errorf("gosmo: set resource governor classifier %q: %w", name, ErrSchemaRequired)
		}
		fn = quoteIdent(schema) + "." + quoteIdent(name)
	}
	if err := rg.alter(ctx, "WITH (CLASSIFIER_FUNCTION = "+fn+")", "set resource governor classifier"); err != nil {
		return err
	}
	if name == "" {
		schema = ""
		setIfApplied(ctx, &rg.ClassifierFunctionID, 0)
	}
	setIfApplied(ctx, &rg.ClassifierSchema, schema)
	setIfApplied(ctx, &rg.ClassifierName, name)
	return nil
}

// SetMaxOutstandingIOPerVolume sets the stored MAX_OUTSTANDING_IO_PER_VOLUME
// (1–100). 0 restores the default, letting the server choose — sent as
// DEFAULT, since the server refuses a literal 0.
func (rg *ResourceGovernor) SetMaxOutstandingIOPerVolume(ctx context.Context, n int) error {
	v := "DEFAULT"
	if n != 0 {
		v = strconv.Itoa(n)
	}
	if err := rg.alter(ctx, "WITH (MAX_OUTSTANDING_IO_PER_VOLUME = "+v+")",
		"set resource governor max outstanding I/O per volume"); err != nil {
		return err
	}
	setIfApplied(ctx, &rg.MaxOutstandingIOPerVolume, n)
	return nil
}

// ClassifierFunction names a function in master that can be the Resource
// Governor classifier.
type ClassifierFunction struct {
	Schema string
	Name   string
}

// ClassifierFunctionCandidates lists the functions in master that
// SetClassifier accepts: schema-bound scalar functions with no parameters
// returning sysname. The current classifier is among them. It reads master's
// catalog by three-part name, so the connection's current database does not
// matter; a login that cannot see the functions gets an empty list.
func (s *Server) ClassifierFunctionCandidates(ctx context.Context) ([]ClassifierFunction, error) {
	rows, err := s.query(ctx, `
SELECT sc.name, o.name
FROM   master.sys.objects o
JOIN   master.sys.schemas sc ON sc.schema_id = o.schema_id
JOIN   master.sys.sql_modules m ON m.object_id = o.object_id
JOIN   master.sys.parameters r ON r.object_id = o.object_id AND r.parameter_id = 0
WHERE  o.type = 'FN' AND m.is_schema_bound = 1
  AND  r.user_type_id = TYPE_ID(N'sysname')
  AND  NOT EXISTS (SELECT 1 FROM master.sys.parameters p
                   WHERE p.object_id = o.object_id AND p.parameter_id > 0)
ORDER  BY sc.name, o.name`)
	return scanRows(rows, err, "list resource governor classifier candidates", func(scan func(...any) error) (ClassifierFunction, error) {
		var f ClassifierFunction
		err := scan(&f.Schema, &f.Name)
		return f, err
	})
}

// -- Resource pools --------------------------------------------------------------

// ResourcePoolOptions is the WITH list of CREATE and ALTER RESOURCE POOL. A
// nil field is left out — at the server default on create, unchanged on
// alter; a non-nil one is sent even when it holds the default.
//
// Affinity is not here: it is read and scripted but not written (the
// scheduler list the DDL takes is not what the catalog stores; see
// scripter_resource_governor.go).
type ResourcePoolOptions struct {
	MinCPUPercent    *int
	MaxCPUPercent    *int
	CapCPUPercent    *int
	MinMemoryPercent *int
	MaxMemoryPercent *int
	MinIOPSPerVolume *int
	MaxIOPSPerVolume *int
}

func (o ResourcePoolOptions) render() *rgOptions {
	w := &rgOptions{}
	w.int("MIN_CPU_PERCENT", o.MinCPUPercent)
	w.int("MAX_CPU_PERCENT", o.MaxCPUPercent)
	w.int("CAP_CPU_PERCENT", o.CapCPUPercent)
	w.int("MIN_MEMORY_PERCENT", o.MinMemoryPercent)
	w.int("MAX_MEMORY_PERCENT", o.MaxMemoryPercent)
	w.int("MIN_IOPS_PER_VOLUME", o.MinIOPSPerVolume)
	w.int("MAX_IOPS_PER_VOLUME", o.MaxIOPSPerVolume)
	return w
}

// CreateResourcePoolRequest describes a new resource pool.
type CreateResourcePoolRequest struct {
	Name    string
	Options ResourcePoolOptions
}

// ResourcePoolRef returns a handle for a resource pool by name, without
// reading the catalog. Every field but Name is zero — so IsSystem is false
// even for default — and ResourcePoolByName is what populates them. Alter
// and Drop address the pool by name, so the handle is enough for both.
func (s *Server) ResourcePoolRef(name string) *ResourcePool {
	return &ResourcePool{server: s, Name: name}
}

// CreateResourcePool creates a resource pool and returns it read back, or,
// under Scripting(ctx), the ResourcePoolRef handle. The pool is not in force
// until ResourceGovernor.Reconfigure.
func (s *Server) CreateResourcePool(ctx context.Context, req CreateResourcePoolRequest) (*ResourcePool, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("gosmo: create resource pool: pool has no name")
	}
	stmt := "CREATE RESOURCE POOL " + quoteIdent(req.Name) + req.Options.render().with()
	if err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create resource pool %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.ResourcePoolRef(req.Name), func() (*ResourcePool, error) {
		return s.ResourcePoolByName(ctx, req.Name)
	})
}

// Alter applies every option set on o in one ALTER RESOURCE POOL. An empty
// o issues nothing. Pending until ResourceGovernor.Reconfigure.
func (p *ResourcePool) Alter(ctx context.Context, o ResourcePoolOptions) error {
	w := o.render()
	if len(w.parts) == 0 {
		return nil
	}
	if err := p.server.exec(ctx, "ALTER RESOURCE POOL "+quoteIdent(p.Name)+w.with()); err != nil {
		return fmt.Errorf("gosmo: alter resource pool %q: %w", p.Name, err)
	}
	setPtrIfApplied(ctx, &p.MinCPUPercent, o.MinCPUPercent)
	setPtrIfApplied(ctx, &p.MaxCPUPercent, o.MaxCPUPercent)
	setPtrIfApplied(ctx, &p.CapCPUPercent, o.CapCPUPercent)
	setPtrIfApplied(ctx, &p.MinMemoryPercent, o.MinMemoryPercent)
	setPtrIfApplied(ctx, &p.MaxMemoryPercent, o.MaxMemoryPercent)
	setPtrIfApplied(ctx, &p.MinIOPSPerVolume, o.MinIOPSPerVolume)
	setPtrIfApplied(ctx, &p.MaxIOPSPerVolume, o.MaxIOPSPerVolume)
	return nil
}

// Drop drops the pool. The server refuses while any workload group still
// uses it (Msg 10916), and always for internal and default.
func (p *ResourcePool) Drop(ctx context.Context) error {
	if err := p.server.exec(ctx, "DROP RESOURCE POOL "+quoteIdent(p.Name)); err != nil {
		return fmt.Errorf("gosmo: drop resource pool %q: %w", p.Name, err)
	}
	return nil
}

// -- Workload groups -------------------------------------------------------------

// WorkloadGroupOptions is the WITH list and USING clause of CREATE and ALTER
// WORKLOAD GROUP. A nil field is left out — at the server default on create,
// unchanged on alter.
type WorkloadGroupOptions struct {
	Importance *WorkloadImportance

	// RequestMaxMemoryGrantPercent may be fractional from SQL Server 2019;
	// before that a fractional value is refused with ErrUnsupportedVersion.
	RequestMaxMemoryGrantPercent *float64
	RequestMaxCPUTimeSec         *int
	RequestMemoryGrantTimeoutSec *int
	MaxDOP                       *int
	GroupMaxRequests             *int

	// GroupMaxTempdbDataPercent and GroupMaxTempdbDataMB set SQL Server
	// 2025's tempdb limits; the Clear flags send NULL, removing one. Setting
	// a value and its Clear flag together is an error. Any of the four
	// before 2025 is refused with ErrUnsupportedVersion.
	GroupMaxTempdbDataPercent      *float64
	GroupMaxTempdbDataMB           *float64
	ClearGroupMaxTempdbDataPercent bool
	ClearGroupMaxTempdbDataMB      bool

	// Pool and ExternalPool are the USING clause. On create, nil means the
	// default pool; on alter, a non-nil one moves the group.
	Pool         *string
	ExternalPool *string
}

// render builds the WITH list and the USING clause (leading space included,
// "" when neither pool is set), checking o against the instance's major.
func (o WorkloadGroupOptions) render(major int) (*rgOptions, string, error) {
	if o.Importance != nil {
		switch imp := strings.ToUpper(string(*o.Importance)); imp {
		case "LOW", "MEDIUM", "HIGH":
		default:
			return nil, "", fmt.Errorf("importance %q is not Low, Medium or High", *o.Importance)
		}
	}
	if v := o.RequestMaxMemoryGrantPercent; v != nil && *v != float64(int64(*v)) &&
		major != 0 && major < int(SQLServer2019) {
		return nil, "", unsupportedVersionf(
			"a fractional request max memory grant percent (%s) needs SQL Server 2019 or later; this instance is major %d",
			formatRGNumber(*v), major)
	}
	if (o.GroupMaxTempdbDataPercent != nil && o.ClearGroupMaxTempdbDataPercent) ||
		(o.GroupMaxTempdbDataMB != nil && o.ClearGroupMaxTempdbDataMB) {
		return nil, "", fmt.Errorf("a tempdb limit is both set and cleared")
	}
	if (o.GroupMaxTempdbDataPercent != nil || o.GroupMaxTempdbDataMB != nil ||
		o.ClearGroupMaxTempdbDataPercent || o.ClearGroupMaxTempdbDataMB) &&
		major != 0 && major < int(SQLServer2025) {
		return nil, "", unsupportedVersionf(
			"tempdb limits on a workload group need SQL Server 2025 or later; this instance is major %d", major)
	}

	w := &rgOptions{}
	if o.Importance != nil {
		w.raw("IMPORTANCE = " + strings.ToUpper(string(*o.Importance)))
	}
	w.float("REQUEST_MAX_MEMORY_GRANT_PERCENT", o.RequestMaxMemoryGrantPercent)
	w.int("REQUEST_MAX_CPU_TIME_SEC", o.RequestMaxCPUTimeSec)
	w.int("REQUEST_MEMORY_GRANT_TIMEOUT_SEC", o.RequestMemoryGrantTimeoutSec)
	w.int("MAX_DOP", o.MaxDOP)
	w.int("GROUP_MAX_REQUESTS", o.GroupMaxRequests)
	w.float("GROUP_MAX_TEMPDB_DATA_PERCENT", o.GroupMaxTempdbDataPercent)
	if o.ClearGroupMaxTempdbDataPercent {
		w.raw("GROUP_MAX_TEMPDB_DATA_PERCENT = NULL")
	}
	w.float("GROUP_MAX_TEMPDB_DATA_MB", o.GroupMaxTempdbDataMB)
	if o.ClearGroupMaxTempdbDataMB {
		w.raw("GROUP_MAX_TEMPDB_DATA_MB = NULL")
	}

	var using []string
	if o.Pool != nil {
		using = append(using, quoteIdent(*o.Pool))
	}
	if o.ExternalPool != nil {
		using = append(using, "EXTERNAL "+quoteIdent(*o.ExternalPool))
	}
	u := ""
	if len(using) > 0 {
		u = " USING " + strings.Join(using, ", ")
	}
	return w, u, nil
}

// CreateWorkloadGroupRequest describes a new workload group. Options.Pool
// names its resource pool; nil puts it in default.
type CreateWorkloadGroupRequest struct {
	Name    string
	Options WorkloadGroupOptions
}

// WorkloadGroupRef returns a handle for a workload group by name, without
// reading the catalog. Every field but Name is zero; WorkloadGroupByName is
// what populates them. Group names are unique server-wide, so the name is
// the whole address and Alter and Drop work from the handle.
func (s *Server) WorkloadGroupRef(name string) *WorkloadGroup {
	return &WorkloadGroup{server: s, Name: name}
}

// CreateWorkloadGroup creates a workload group and returns it read back, or,
// under Scripting(ctx), the WorkloadGroupRef handle. Not in force until
// ResourceGovernor.Reconfigure.
func (s *Server) CreateWorkloadGroup(ctx context.Context, req CreateWorkloadGroupRequest) (*WorkloadGroup, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("gosmo: create workload group: group has no name")
	}
	w, using, err := req.Options.render(s.serverMajorVersion())
	if err != nil {
		return nil, fmt.Errorf("gosmo: create workload group %q: %w", req.Name, err)
	}
	stmt := "CREATE WORKLOAD GROUP " + quoteIdent(req.Name) + w.with() + using
	if err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create workload group %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.WorkloadGroupRef(req.Name), func() (*WorkloadGroup, error) {
		return s.WorkloadGroupByName(ctx, req.Name)
	})
}

// Alter applies every option set on o in one ALTER WORKLOAD GROUP; a
// non-nil Pool or ExternalPool moves the group. An empty o issues nothing.
// Pending until ResourceGovernor.Reconfigure.
//
// Moved pools are mirrored by name only: PoolID and ExternalPoolID are not
// re-read.
func (g *WorkloadGroup) Alter(ctx context.Context, o WorkloadGroupOptions) error {
	w, using, err := o.render(g.server.serverMajorVersion())
	if err != nil {
		return fmt.Errorf("gosmo: alter workload group %q: %w", g.Name, err)
	}
	if len(w.parts) == 0 && using == "" {
		return nil
	}
	if err := g.server.exec(ctx, "ALTER WORKLOAD GROUP "+quoteIdent(g.Name)+w.with()+using); err != nil {
		return fmt.Errorf("gosmo: alter workload group %q: %w", g.Name, err)
	}
	if o.Importance != nil {
		// The catalog spells it Low/Medium/High whatever case was sent.
		imp := strings.ToLower(string(*o.Importance))
		setIfApplied(ctx, &g.Importance, strings.ToUpper(imp[:1])+imp[1:])
	}
	setPtrIfApplied(ctx, &g.RequestMaxMemoryGrantPercent, o.RequestMaxMemoryGrantPercent)
	setPtrIfApplied(ctx, &g.RequestMaxCPUTimeSec, o.RequestMaxCPUTimeSec)
	setPtrIfApplied(ctx, &g.RequestMemoryGrantTimeoutSec, o.RequestMemoryGrantTimeoutSec)
	setPtrIfApplied(ctx, &g.MaxDOP, o.MaxDOP)
	setPtrIfApplied(ctx, &g.GroupMaxRequests, o.GroupMaxRequests)
	if o.GroupMaxTempdbDataPercent != nil {
		setIfApplied(ctx, &g.GroupMaxTempdbDataPercent, new(*o.GroupMaxTempdbDataPercent))
	}
	if o.ClearGroupMaxTempdbDataPercent {
		setIfApplied(ctx, &g.GroupMaxTempdbDataPercent, nil)
	}
	if o.GroupMaxTempdbDataMB != nil {
		setIfApplied(ctx, &g.GroupMaxTempdbDataMB, new(*o.GroupMaxTempdbDataMB))
	}
	if o.ClearGroupMaxTempdbDataMB {
		setIfApplied(ctx, &g.GroupMaxTempdbDataMB, nil)
	}
	setPtrIfApplied(ctx, &g.PoolName, o.Pool)
	setPtrIfApplied(ctx, &g.ExternalPoolName, o.ExternalPool)
	return nil
}

// Drop drops the workload group. The server refuses for internal and
// default. Sessions already classified into the group keep running in it
// until they end.
func (g *WorkloadGroup) Drop(ctx context.Context) error {
	if err := g.server.exec(ctx, "DROP WORKLOAD GROUP "+quoteIdent(g.Name)); err != nil {
		return fmt.Errorf("gosmo: drop workload group %q: %w", g.Name, err)
	}
	return nil
}

// -- External resource pools -----------------------------------------------------

// ExternalResourcePoolOptions is the WITH list of CREATE and ALTER EXTERNAL
// RESOURCE POOL. A nil field is left out. Affinity is read and scripted, not
// written, as for ResourcePoolOptions.
type ExternalResourcePoolOptions struct {
	MaxCPUPercent    *int
	MaxMemoryPercent *int
	MaxProcesses     *int
}

func (o ExternalResourcePoolOptions) render() *rgOptions {
	w := &rgOptions{}
	w.int("MAX_CPU_PERCENT", o.MaxCPUPercent)
	w.int("MAX_MEMORY_PERCENT", o.MaxMemoryPercent)
	w.int("MAX_PROCESSES", o.MaxProcesses)
	return w
}

// CreateExternalResourcePoolRequest describes a new external resource pool.
type CreateExternalResourcePoolRequest struct {
	Name    string
	Options ExternalResourcePoolOptions
}

// ExternalResourcePoolRef returns a handle for an external resource pool by
// name, without reading the catalog. Every field but Name is zero;
// ExternalResourcePoolByName is what populates them.
func (s *Server) ExternalResourcePoolRef(name string) *ExternalResourcePool {
	return &ExternalResourcePool{server: s, Name: name}
}

// CreateExternalResourcePool creates an external resource pool and returns
// it read back, or, under Scripting(ctx), the ExternalResourcePoolRef handle.
func (s *Server) CreateExternalResourcePool(ctx context.Context, req CreateExternalResourcePoolRequest) (*ExternalResourcePool, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("gosmo: create external resource pool: pool has no name")
	}
	stmt := "CREATE EXTERNAL RESOURCE POOL " + quoteIdent(req.Name) + req.Options.render().with()
	if err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create external resource pool %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.ExternalResourcePoolRef(req.Name), func() (*ExternalResourcePool, error) {
		return s.ExternalResourcePoolByName(ctx, req.Name)
	})
}

// Alter applies every option set on o in one ALTER EXTERNAL RESOURCE POOL.
// An empty o issues nothing. The default external pool accepts it.
func (p *ExternalResourcePool) Alter(ctx context.Context, o ExternalResourcePoolOptions) error {
	w := o.render()
	if len(w.parts) == 0 {
		return nil
	}
	if err := p.server.exec(ctx, "ALTER EXTERNAL RESOURCE POOL "+quoteIdent(p.Name)+w.with()); err != nil {
		return fmt.Errorf("gosmo: alter external resource pool %q: %w", p.Name, err)
	}
	setPtrIfApplied(ctx, &p.MaxCPUPercent, o.MaxCPUPercent)
	setPtrIfApplied(ctx, &p.MaxMemoryPercent, o.MaxMemoryPercent)
	setPtrIfApplied(ctx, &p.MaxProcesses, o.MaxProcesses)
	return nil
}

// Drop drops the external pool. The server refuses for default, and while a
// workload group still uses the pool.
func (p *ExternalResourcePool) Drop(ctx context.Context) error {
	if err := p.server.exec(ctx, "DROP EXTERNAL RESOURCE POOL "+quoteIdent(p.Name)); err != nil {
		return fmt.Errorf("gosmo: drop external resource pool %q: %w", p.Name, err)
	}
	return nil
}
