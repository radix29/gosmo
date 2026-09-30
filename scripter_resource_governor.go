package gosmo

import (
	"context"
	"fmt"
	"math/bits"
	"strings"
)

// ============================================================
// ServerScripter — Resource Governor
// ============================================================

// scripter_resource_governor.go scripts resource pools, workload groups,
// external resource pools and the Resource Governor configuration itself.
//
// Only options that differ from the server default are written, so a script
// says what was chosen rather than restating the defaults.
//
// A per-object script does not end in ALTER RESOURCE GOVERNOR RECONFIGURE,
// though the object is not in force without one: RECONFIGURE also enables a
// disabled governor, so appending it to "script this pool" would turn the
// governor on for whoever runs the script. A comment says what applies it.
// ScriptResourceGovernor, the configuration's own script, is where
// RECONFIGURE (or DISABLE) belongs, chosen from the stored IsEnabled.
//
// # Built-in objects
//
// internal and default (and the default external pool) always exist and
// cannot be created or dropped. CREATE for one scripts an ALTER of its
// non-default options — or, with none, a comment saying so; internal's are
// always the defaults, since it cannot be altered. DROP is refused with
// ErrUnsupported: there is no statement to write.
//
// # Affinity
//
// The catalog stores affinity as a bit mask per processor group, and the DDL
// takes scheduler (or CPU) ids. In processor group 0 bit n is id n. Beyond
// group 0 the ids are numbered on from the previous group's *actual* size,
// which the catalog does not record, so a pool with affinity outside group 0
// is refused with ErrUnsupported rather than scripted onto the wrong
// schedulers. A pool created with AFFINITY NUMANODE is stored as its
// schedulers and scripts as AFFINITY SCHEDULER — the same placement.

const rgReconfigureNote = "-- Takes effect after ALTER RESOURCE GOVERNOR RECONFIGURE, which also enables a disabled governor.\n"

// ScriptResourceGovernor scripts the stored Resource Governor configuration:
// the classifier and MAX_OUTSTANDING_IO_PER_VOLUME where set, then
// RECONFIGURE when the governor is enabled or DISABLE when it is not. The
// classifier function itself is not scripted — it is a function in master,
// scripted with the rest of master's modules. DROP has no meaning for the
// configuration and is refused with ErrUnsupported.
func (sc *ServerScripter) ScriptResourceGovernor(ctx context.Context) (string, error) {
	rg, err := sc.server.ResourceGovernor(ctx)
	if err != nil {
		return "", err
	}
	return buildResourceGovernorScript(rg, sc.opts)
}

func buildResourceGovernorScript(rg *ResourceGovernor, opts ScriptOptions) (string, error) {
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		return "", unsupportedf("gosmo: script resource governor: the configuration cannot be dropped")
	}
	var sb strings.Builder
	if rg.ClassifierName != "" {
		fmt.Fprintf(&sb, "ALTER RESOURCE GOVERNOR WITH (CLASSIFIER_FUNCTION = %s.%s);\nGO\n",
			quoteIdent(rg.ClassifierSchema), quoteIdent(rg.ClassifierName))
	}
	if rg.MaxOutstandingIOPerVolume != 0 {
		fmt.Fprintf(&sb, "ALTER RESOURCE GOVERNOR WITH (MAX_OUTSTANDING_IO_PER_VOLUME = %d);\nGO\n",
			rg.MaxOutstandingIOPerVolume)
	}
	if rg.IsEnabled {
		sb.WriteString("ALTER RESOURCE GOVERNOR RECONFIGURE;\nGO\n")
	} else {
		sb.WriteString("ALTER RESOURCE GOVERNOR DISABLE;\nGO\n")
	}
	return sb.String(), nil
}

// affinityList renders group-0 masks as a DDL id list — "0 TO 3, 6" — or
// refuses one reaching past processor group 0 (see the file comment).
func affinityList(what string, groups []int, masks []int64) (string, error) {
	var ids []string
	for i, g := range groups {
		if masks[i] == 0 {
			continue
		}
		if g != 0 {
			return "", unsupportedf("gosmo: script %s: affinity in processor group %d cannot be mapped to scheduler ids from the catalog", what, g)
		}
		m := uint64(masks[i])
		for m != 0 {
			lo := bits.TrailingZeros64(m)
			hi := lo
			for hi+1 < 64 && m&(1<<(hi+1)) != 0 {
				hi++
			}
			if hi == lo {
				ids = append(ids, fmt.Sprint(lo))
			} else {
				ids = append(ids, fmt.Sprintf("%d TO %d", lo, hi))
			}
			for b := lo; b <= hi; b++ {
				m &^= 1 << b
			}
		}
	}
	return strings.Join(ids, ", "), nil
}

// systemRGScript is the CREATE half for a built-in object: an ALTER of what
// differs from the defaults, or a comment when nothing does.
func systemRGScript(sb *strings.Builder, alter, kind, name string, w *rgOptions, using string) {
	if len(w.parts) == 0 && using == "" {
		fmt.Fprintf(sb, "-- %s %s is built in and has its default settings.\n", kind, quoteIdent(name))
		return
	}
	fmt.Fprintf(sb, "%s %s%s%s;\nGO\n", alter, quoteIdent(name), w.with(), using)
}

func refuseSystemDrop(opts ScriptOptions, kind, name string) error {
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		return unsupportedf("gosmo: script %s %q: a built-in %s cannot be dropped", kind, name, kind)
	}
	return nil
}

// -- Resource pools --------------------------------------------------------------

// ScriptResourcePool generates the CREATE (or DROP) script for one resource
// pool.
func (sc *ServerScripter) ScriptResourcePool(ctx context.Context, name string) (string, error) {
	p, err := sc.server.ResourcePoolByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildResourcePoolScript(p, sc.opts)
}

// nonDefaultOptions is the pool's WITH list with the server defaults left out.
func (p *ResourcePool) nonDefaultOptions() ResourcePoolOptions {
	var o ResourcePoolOptions
	for _, f := range []struct {
		dst      **int
		v, deflt int
	}{
		{&o.MinCPUPercent, p.MinCPUPercent, 0},
		{&o.MaxCPUPercent, p.MaxCPUPercent, 100},
		{&o.CapCPUPercent, p.CapCPUPercent, 100},
		{&o.MinMemoryPercent, p.MinMemoryPercent, 0},
		{&o.MaxMemoryPercent, p.MaxMemoryPercent, 100},
		{&o.MinIOPSPerVolume, p.MinIOPSPerVolume, 0},
		{&o.MaxIOPSPerVolume, p.MaxIOPSPerVolume, 0},
	} {
		if f.v != f.deflt {
			*f.dst = new(f.v)
		}
	}
	return o
}

func buildResourcePoolScript(p *ResourcePool, opts ScriptOptions) (string, error) {
	w := p.nonDefaultOptions().render()
	groups, masks := make([]int, len(p.Affinity)), make([]int64, len(p.Affinity))
	for i, a := range p.Affinity {
		groups[i], masks[i] = a.ProcessorGroup, a.SchedulerMask
	}
	aff, err := affinityList(fmt.Sprintf("resource pool %q", p.Name), groups, masks)
	if err != nil {
		return "", err
	}
	if aff != "" {
		w.raw("AFFINITY SCHEDULER = (" + aff + ")")
	}
	if p.IsSystem() {
		if err := refuseSystemDrop(opts, "resource pool", p.Name); err != nil {
			return "", err
		}
		var sb strings.Builder
		systemRGScript(&sb, "ALTER RESOURCE POOL", "resource pool", p.Name, w, "")
		return sb.String(), nil
	}
	drop := fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.resource_governor_resource_pools WHERE name = N'%s')\n    DROP RESOURCE POOL %s;\nGO\n",
		escapeSingle(p.Name), quoteIdent(p.Name))
	guard := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_resource_pools WHERE name = N'%s')\n",
		escapeSingle(p.Name))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE RESOURCE POOL %s%s;\nGO\n%s", quoteIdent(p.Name), w.with(), rgReconfigureNote)
	}), nil
}

// -- Workload groups -------------------------------------------------------------

// ScriptWorkloadGroup generates the CREATE (or DROP) script for one workload
// group. The pools it uses are not scripted with it.
func (sc *ServerScripter) ScriptWorkloadGroup(ctx context.Context, name string) (string, error) {
	g, err := sc.server.WorkloadGroupByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildWorkloadGroupScript(g, sc.opts)
}

// nonDefaultOptions is the group's WITH list and USING pools with the server
// defaults left out. An external pool name the login could not resolve ("")
// is taken as default.
func (g *WorkloadGroup) nonDefaultOptions() WorkloadGroupOptions {
	var o WorkloadGroupOptions
	if g.Importance != "" && !strings.EqualFold(g.Importance, string(ImportanceMedium)) {
		o.Importance = new(WorkloadImportance(g.Importance))
	}
	if g.RequestMaxMemoryGrantPercent != 25 {
		o.RequestMaxMemoryGrantPercent = new(g.RequestMaxMemoryGrantPercent)
	}
	for _, f := range []struct {
		dst **int
		v   int
	}{
		{&o.RequestMaxCPUTimeSec, g.RequestMaxCPUTimeSec},
		{&o.RequestMemoryGrantTimeoutSec, g.RequestMemoryGrantTimeoutSec},
		{&o.MaxDOP, g.MaxDOP},
		{&o.GroupMaxRequests, g.GroupMaxRequests},
	} {
		if f.v != 0 {
			*f.dst = new(f.v)
		}
	}
	o.GroupMaxTempdbDataPercent = g.GroupMaxTempdbDataPercent
	o.GroupMaxTempdbDataMB = g.GroupMaxTempdbDataMB
	if g.PoolName != "" && g.PoolName != "default" {
		o.Pool = new(g.PoolName)
	}
	if g.ExternalPoolName != "" && g.ExternalPoolName != "default" {
		if o.Pool == nil {
			// USING EXTERNAL x alone is not the grammar; name the pool too.
			o.Pool = new("default")
		}
		o.ExternalPool = new(g.ExternalPoolName)
	}
	return o
}

func buildWorkloadGroupScript(g *WorkloadGroup, opts ScriptOptions) (string, error) {
	// Major 0: the values came from this server's own catalog, so they are
	// already ones it accepts; no version check applies to them.
	w, using, err := g.nonDefaultOptions().render(0)
	if err != nil {
		return "", fmt.Errorf("gosmo: script workload group %q: %w", g.Name, err)
	}
	if g.IsSystem() {
		if err := refuseSystemDrop(opts, "workload group", g.Name); err != nil {
			return "", err
		}
		var sb strings.Builder
		systemRGScript(&sb, "ALTER WORKLOAD GROUP", "workload group", g.Name, w, using)
		return sb.String(), nil
	}
	drop := fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.resource_governor_workload_groups WHERE name = N'%s')\n    DROP WORKLOAD GROUP %s;\nGO\n",
		escapeSingle(g.Name), quoteIdent(g.Name))
	guard := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_workload_groups WHERE name = N'%s')\n",
		escapeSingle(g.Name))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE WORKLOAD GROUP %s%s%s;\nGO\n%s", quoteIdent(g.Name), w.with(), using, rgReconfigureNote)
	}), nil
}

// -- External resource pools -----------------------------------------------------

// ScriptExternalResourcePool generates the CREATE (or DROP) script for one
// external resource pool.
func (sc *ServerScripter) ScriptExternalResourcePool(ctx context.Context, name string) (string, error) {
	p, err := sc.server.ExternalResourcePoolByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildExternalResourcePoolScript(p, sc.opts)
}

// nonDefaultOptions is the external pool's WITH list with the server defaults
// (100% CPU, 20% memory, unlimited processes — read off the default external
// pool on 17) left out.
func (p *ExternalResourcePool) nonDefaultOptions() ExternalResourcePoolOptions {
	var o ExternalResourcePoolOptions
	if p.MaxCPUPercent != 100 {
		o.MaxCPUPercent = new(p.MaxCPUPercent)
	}
	if p.MaxMemoryPercent != 20 {
		o.MaxMemoryPercent = new(p.MaxMemoryPercent)
	}
	if p.MaxProcesses != 0 {
		o.MaxProcesses = new(p.MaxProcesses)
	}
	return o
}

func buildExternalResourcePoolScript(p *ExternalResourcePool, opts ScriptOptions) (string, error) {
	w := p.nonDefaultOptions().render()
	groups, masks := make([]int, len(p.Affinity)), make([]int64, len(p.Affinity))
	for i, a := range p.Affinity {
		groups[i], masks[i] = a.ProcessorGroup, a.CPUMask
	}
	aff, err := affinityList(fmt.Sprintf("external resource pool %q", p.Name), groups, masks)
	if err != nil {
		return "", err
	}
	if aff != "" {
		w.raw("AFFINITY CPU = (" + aff + ")")
	}
	if p.IsSystem() {
		if err := refuseSystemDrop(opts, "external resource pool", p.Name); err != nil {
			return "", err
		}
		var sb strings.Builder
		systemRGScript(&sb, "ALTER EXTERNAL RESOURCE POOL", "external resource pool", p.Name, w, "")
		return sb.String(), nil
	}
	drop := fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.resource_governor_external_resource_pools WHERE name = N'%s')\n    DROP EXTERNAL RESOURCE POOL %s;\nGO\n",
		escapeSingle(p.Name), quoteIdent(p.Name))
	guard := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_external_resource_pools WHERE name = N'%s')\n",
		escapeSingle(p.Name))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE EXTERNAL RESOURCE POOL %s%s;\nGO\n%s", quoteIdent(p.Name), w.with(), rgReconfigureNote)
	}), nil
}
