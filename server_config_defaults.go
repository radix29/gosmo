package gosmo

// ============================================================
// Configuration defaults  (the engine's own sp_configure defaults)
// ============================================================

// configDefault is one row of the defaults table: the option's
// sys.configurations name, its engine default, and the majors it applies to —
// since inclusive, until exclusive (0 = still current). An option whose
// default changed between majors has one row per range.
type configDefault struct {
	name  string
	value int64
	since ServerVersion
	until ServerVersion
}

// configDefaults is the engine default of every sp_configure option, from
// Microsoft's "Server configuration options" reference (2026-10-21 revision)
// and the per-option pages it links, checked against the 13, 14 and 17
// instances' sys.configurations.
//
// It is the **engine** default, the value a bare installation of the engine
// carries, not Setup's recommendation: max degree of parallelism 0 and max
// server memory 2147483647, though Setup has proposed both since 2019 (and
// mssql-conf on Linux); Agent XPs 0, though starting SQL Server Agent turns it
// on. A configured instance differs from it in places by design.
//
// Left out on purpose, so their ConfigurationOption.HasDefault is false:
//   - "version high part of SQL Server" and "version low part of SQL
//     Server" (2019+): the engine writes them, they are not settings, and the
//     reference's 0 is not what any instance holds.
//   - Options the reference does not list, all seen on the 17.0.1145
//     instance: "ADR cleaner lock timeout (s)", "allow filesystem
//     enumeration", "external AI runtimes enabled", "max server tiered memory
//     (MB)", "openrowset auto_create_statistics", "SLOG memory quota (%)" and
//     "tiered memory enabled". A default copied from an instance that is not
//     a fresh install would be a guess.
//
// Majors 15 and 16 come from the reference alone — no instance was at hand.
// Where it says only "Yes" for an option the 14 instance lacks, since is the
// first major it is documented on per-option, else 15 when it appeared with
// 2019's ADR-era options, else 17 (seen live there, nowhere older).
var configDefaults = []configDefault{
	{"access check cache bucket count", 0, SQLServer2016, 0},
	{"access check cache quota", 0, SQLServer2016, 0},
	{"Ad Hoc Distributed Queries", 0, SQLServer2016, 0},
	{"ADR cleaner retry timeout (min)", 120, SQLServer2019, SQLServer2022},
	{"ADR cleaner retry timeout (min)", 15, SQLServer2022, 0},
	{"ADR Cleaner Thread Count", 1, SQLServer2019, 0},
	{"ADR Preallocation Factor", 4, SQLServer2019, 0},
	{"affinity I/O mask", 0, SQLServer2016, 0},
	{"affinity mask", 0, SQLServer2016, 0},
	{"affinity64 I/O mask", 0, SQLServer2016, 0},
	{"affinity64 mask", 0, SQLServer2016, 0},
	{"Agent XPs", 0, SQLServer2016, 0},
	{"allow polybase export", 0, SQLServer2016, 0},
	{"allow server scoped db credentials", 0, SQLServer2025, 0},
	{"allow updates", 0, SQLServer2016, 0},
	{"automatic soft-NUMA disabled", 0, SQLServer2016, 0},
	{"availability group commit time (ms)", 0, SQLServer2025, 0},
	{"backup checksum default", 0, SQLServer2016, 0},
	{"backup compression algorithm", 0, SQLServer2022, 0},
	{"backup compression default", 0, SQLServer2016, 0},
	{"blocked process threshold (s)", 0, SQLServer2016, 0},
	{"c2 audit mode", 0, SQLServer2016, 0},
	{"clr enabled", 0, SQLServer2016, 0},
	{"clr strict security", 1, SQLServer2017, 0},
	{"column encryption enclave type", 0, SQLServer2019, 0},
	{"common criteria compliance enabled", 0, SQLServer2016, 0},
	{"contained database authentication", 0, SQLServer2016, 0},
	{"cost threshold for parallelism", 5, SQLServer2016, 0},
	{"cross db ownership chaining", 0, SQLServer2016, 0},
	{"cursor threshold", -1, SQLServer2016, 0},
	{"Data processed daily limit in TB", 2147483647, SQLServer2019, 0},
	{"Data processed monthly limit in TB", 2147483647, SQLServer2019, 0},
	{"Data processed weekly limit in TB", 2147483647, SQLServer2019, 0},
	{"Database Mail XPs", 0, SQLServer2016, 0},
	{"default full-text language", 1033, SQLServer2016, 0},
	{"default language", 0, SQLServer2016, 0},
	{"default trace enabled", 1, SQLServer2016, 0},
	{"disallow results from triggers", 0, SQLServer2016, 0},
	{"EKM provider enabled", 0, SQLServer2016, 0},
	{"external rest endpoint enabled", 0, SQLServer2025, 0},
	{"external scripts enabled", 0, SQLServer2016, 0},
	{"external xtp dll gen util enabled", 0, SQLServer2022, 0},
	{"filestream access level", 0, SQLServer2016, 0},
	{"fill factor (%)", 0, SQLServer2016, 0},
	{"ft crawl bandwidth (max)", 100, SQLServer2016, 0},
	{"ft crawl bandwidth (min)", 0, SQLServer2016, 0},
	{"ft notify bandwidth (max)", 100, SQLServer2016, 0},
	{"ft notify bandwidth (min)", 0, SQLServer2016, 0},
	{"hadoop connectivity", 0, SQLServer2016, 0},
	{"hardware offload config", 0, SQLServer2022, 0},
	{"hardware offload enabled", 0, SQLServer2022, 0},
	{"hardware offload mode", 0, SQLServer2022, 0},
	{"in-doubt xact resolution", 0, SQLServer2016, 0},
	{"index create memory (KB)", 0, SQLServer2016, 0},
	{"lightweight pooling", 0, SQLServer2016, 0},
	{"locks", 0, SQLServer2016, 0},
	{"max degree of parallelism", 0, SQLServer2016, 0},
	{"max full-text crawl range", 4, SQLServer2016, 0},
	{"max lock manager cache memory (%)", 20, SQLServer2025, 0}, // 2025 CU 5
	{"max RPC request params (KB)", 0, SQLServer2019, 0},        // 2019 CU 26, 2022 CU 13
	{"max server memory (MB)", 2147483647, SQLServer2016, 0},
	{"max text repl size (B)", 65536, SQLServer2016, 0},
	{"max UCS send boxcars", 256, SQLServer2025, 0},
	{"max worker threads", 0, SQLServer2016, 0},
	{"media retention", 0, SQLServer2016, 0},
	{"min memory per query (KB)", 1024, SQLServer2016, 0},
	{"min server memory (MB)", 0, SQLServer2016, 0},
	{"nested triggers", 1, SQLServer2016, 0},
	{"network packet size (B)", 4096, SQLServer2016, 0},
	{"Ole Automation Procedures", 0, SQLServer2016, 0},
	{"open objects", 0, SQLServer2016, 0},
	{"optimize for ad hoc workloads", 0, SQLServer2016, 0},
	{"PH timeout (s)", 60, SQLServer2016, 0},
	{"polybase enabled", 0, SQLServer2019, 0},
	{"polybase network encryption", 1, SQLServer2016, 0},
	{"precompute rank", 0, SQLServer2016, 0},
	{"priority boost", 0, SQLServer2016, 0},
	{"query governor cost limit", 0, SQLServer2016, 0},
	{"query wait (s)", -1, SQLServer2016, 0},
	{"recovery interval (min)", 0, SQLServer2016, 0},
	{"remote access", 1, SQLServer2016, 0},
	{"remote admin connections", 0, SQLServer2016, 0},
	{"remote data archive", 0, SQLServer2016, 0},
	{"remote login timeout (s)", 10, SQLServer2016, 0},
	{"remote proc trans", 0, SQLServer2016, 0},
	{"remote query timeout (s)", 600, SQLServer2016, 0},
	{"Replication XPs", 0, SQLServer2016, 0},
	{"scan for startup procs", 0, SQLServer2016, 0},
	{"server trigger recursion", 1, SQLServer2016, 0},
	{"set working set size", 0, SQLServer2016, 0},
	{"show advanced options", 0, SQLServer2016, 0},
	{"SMO and DMO XPs", 1, SQLServer2016, 0},
	{"suppress recovery model errors", 0, SQLServer2025, 0},
	{"tempdb metadata memory-optimized", 0, SQLServer2019, 0},
	{"transform noise words", 0, SQLServer2016, 0},
	{"two digit year cutoff", 2049, SQLServer2016, 0},
	{"user connections", 0, SQLServer2016, 0},
	{"user options", 0, SQLServer2016, 0},
	{"xp_cmdshell", 0, SQLServer2016, 0},
}

// newestDefaultsMajor is the newest major the defaults table was built for.
// A later one gets no defaults: a new major adds options and can change a
// default (ADR cleaner retry timeout went from 120 to 15 in 2022), and a
// stale table would report a changed default as a non-default value.
const newestDefaultsMajor = SQLServer2025

// configDefaultsByMajor is configDefaults resolved per supported major.
var configDefaultsByMajor = func() map[int]map[string]int64 {
	out := make(map[int]map[string]int64)
	for m := MinimumServerVersion; m <= newestDefaultsMajor; m++ {
		byName := make(map[string]int64)
		for _, d := range configDefaults {
			if majorKnownAtLeast(int(m), d.since) && (d.until == 0 || majorBefore(int(m), d.until)) {
				byName[d.name] = d.value
			}
		}
		out[int(m)] = byName
	}
	return out
}()

// configurationDefault is name's engine default on major (a feature major:
// 0 for Azure), false when the table has none — an unsupported or newer
// major, Azure (whose defaults are not documented per option), or an option
// the table leaves out. name is the exact sys.configurations name.
func configurationDefault(major int, name string) (int64, bool) {
	v, ok := configDefaultsByMajor[major][name]
	return v, ok
}

// fillDefault sets c.Default and c.HasDefault from the table for c's server.
func (c *ConfigurationOption) fillDefault() {
	c.Default, c.HasDefault = configurationDefault(c.server.serverMajorVersion(), c.Name)
}
