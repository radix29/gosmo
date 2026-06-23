# gosmo

A Go library that mimics **Microsoft SQL Server Management Objects (SMO)** — without WMI, COM, or Windows-only dependencies.

```
go get github.com/radix29/gosmo
```

> **Go version note:** The module requires Go 1.23+. Go 1.26.4 does not yet exist (latest release at time of writing is 1.23.x); update `go.mod` once that version ships.

---

## Packages

| Path | Purpose |
|------|---------|
| `smo/` | All SMO types and logic |
| `examples/` | Full end-to-end demo |

---

## Quick start

```go
import "github.com/radix29/gosmo"

srv, err := smo.Connect(smo.ConnectionOptions{
    Server:                 "localhost:1433",
    User:                   "sa",
    Password:               "YourPassword",
    TrustServerCertificate: true,
})
if err != nil { log.Fatal(err) }
defer srv.Close()

fmt.Println(srv.Info().ProductVersion)
```

---

## Feature map

### Server
| SMO equivalent | gosmo |
|---|---|
| `Server.Databases` | `srv.Databases()` |
| `Server.Logins` | `srv.Logins()` |
| `Server.Roles` | `srv.ServerRoles()` |
| `Server.LinkedServers` | `srv.LinkedServers()` |
| `Server.Configuration` | `srv.Configurations()` |
| `Server.JobServer.Jobs` | `srv.Jobs()` |
| Active sessions | `srv.ActiveSessions(includeSystem)` |
| Kill session | `srv.KillSession(id)` |
| Error log | `srv.ReadErrorLog(n)` |
| Database Mail | `srv.MailProfiles()` / `srv.SendMail(...)` |

### Database
| SMO equivalent | gosmo |
|---|---|
| `Database.Tables` | `db.Tables()` / `db.TablesBySchema(schema)` |
| `Database.Views` | `db.Views()` |
| `Database.StoredProcedures` | `db.StoredProcedures()` |
| `Database.UserDefinedFunctions` | `db.UserDefinedFunctions()` |
| `Database.Schemas` | `db.Schemas()` |
| `Database.Users` | `db.Users()` |
| `Database.Roles` | `db.DatabaseRoles()` |
| `Database.FileGroups` | `db.FileGroups()` |
| `Database.Triggers` | `db.Triggers()` |
| `Database.Sequences` | `db.Sequences()` |
| `Database.Synonyms` | `db.Synonyms()` |
| Partition functions | `db.PartitionFunctions()` |
| Partition schemes | `db.PartitionSchemes()` |
| Extended properties | `db.ExtendedProperties(level)` |
| Column master keys | `db.ColumnMasterKeys()` |
| Column encryption keys | `db.ColumnEncryptionKeys()` |
| Security policies (RLS) | `db.SecurityPolicies()` |
| `Database.RecoveryModel` | `db.SetRecoveryModel(model)` |
| `Database.CompatibilityLevel` | `db.SetCompatibilityLevel(level)` |
| Space used | `db.SpaceUsed()` |

### Table
| SMO equivalent | gosmo |
|---|---|
| `Table.Columns` | `t.Columns()` |
| `Table.Indexes` | `t.Indexes()` |
| `Table.ForeignKeys` | `t.ForeignKeys()` |
| `Table.Checks` | `t.CheckConstraints()` |
| `Table.Statistics` | `t.Statistics()` |
| `Table.Partitions` | `t.Partitions()` |
| `Table.RowCount` | `t.RowCount()` |
| Truncate | `t.TruncateTable()` |
| Fragmentation | `t.FragmentationStats(mode)` |
| Rebuild all indexes | `t.RebuildAllIndexes(fillFactor)` |
| Update all statistics | `t.UpdateAllStatistics(samplePct)` |
| Create index | `t.CreateIndex(req)` |

### Index
| gosmo |
|---|
| `idx.Rebuild(t, fillFactor)` |
| `idx.Reorganize(t)` |
| `idx.Disable(t)` / `idx.Enable(t)` |
| `idx.Drop(t)` |

### Scripter
```go
sc := smo.NewScripter(db, smo.DefaultScriptOptions())
ddl, _ := sc.ScriptTable("dbo", "MyTable")
ddl, _ := sc.ScriptView("dbo", "MyView")
ddl, _ := sc.ScriptStoredProcedure("dbo", "MyProc")
ddl, _ := sc.ScriptFunction("dbo", "MyFunc")
ddl, _ := sc.ScriptDatabase()
```

### Backup & Restore
```go
srv.Backup(smo.BackupOptions{
    Database: "MyDB",
    Devices:  []string{`C:\Backups\MyDB.bak`},
    CopyOnly: true,
})

srv.Restore(smo.RestoreOptions{
    Database: "MyDB_Restored",
    Devices:  []string{`C:\Backups\MyDB.bak`},
    RelocateFiles: []smo.RelocateFile{
        {LogicalName: "MyDB",     PhysicalName: `C:\Data\MyDB.mdf`},
        {LogicalName: "MyDB_log", PhysicalName: `C:\Data\MyDB.ldf`},
    },
    Recovery: true,
    Replace:  true,
})
```

### Agent Jobs
```go
job, _ := srv.CreateJob(smo.CreateJobRequest{Name: "NightlyBackup", Enabled: true})
job.AddStep(smo.JobStepRequest{
    Name:      "Run backup",
    Subsystem: "TSQL",
    Command:   "EXEC dbo.RunNightlyBackup",
    Database:  "MyDB",
    OnSuccessAction: 1,
    OnFailAction:    2,
})
job.AddSchedule(smo.JobScheduleRequest{
    Name:              "Every night at 2am",
    Enabled:           true,
    FreqType:          4,    // daily
    FreqInterval:      1,
    FreqSubdayType:    1,    // once
    ActiveStartTime:   20000, // 02:00:00
})
job.Start("")
```

---

## Running the example

```bash
export MSSQL_SERVER="localhost:1433"
export MSSQL_USER="sa"
export MSSQL_PASSWORD="YourPassword"
go run ./examples/main.go
```

---

## Features intentionally excluded (require WMI / COM / OS APIs)

- Hardware enumeration (disk, NIC, CPU details beyond what `sys.dm_os_sys_info` provides)
- SQL Server service start/stop/restart
- Performance counters via Windows PDH
- SQL Server Browser service interaction
- Windows Event Log reading
- Registry reads for SQL Server configuration outside `sys.configurations`

All of the above require WMI or Windows-only APIs and are out of scope for a cross-platform Go library.
