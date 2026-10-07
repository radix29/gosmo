package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The fixture's Customer article carries 0x30073 (seen on 2025): every bit
// named, lowest first, and an undocumented bit reported by value rather than
// dropped.
func TestSchemaOptionOptions(t *testing.T) {
	got := SchemaOption(0x30073).Options()
	want := []string{
		"Create the object (CREATE TABLE, CREATE PROCEDURE, …)",
		"Generate the change-propagation stored procedures",
		"Copy the clustered index",
		"Convert user-defined types to base types",
		"Copy nonclustered indexes",
		"Copy check constraints as NOT FOR REPLICATION",
		"Copy foreign key constraints as NOT FOR REPLICATION",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Options() = %q, want %q", got, want)
	}
	if got := SchemaOption(0x200000000000 | 0x01).Options(); len(got) != 2 || got[1] != "0x200000000000" {
		t.Errorf("undocumented bit: Options() = %q", got)
	}
	if got := SchemaOption(0).Options(); got != nil {
		t.Errorf("zero: Options() = %q, want none", got)
	}
}

func TestSchemaOptionBitsAreDistinctAndAscending(t *testing.T) {
	var prev SchemaOption
	for _, b := range schemaOptionBits {
		if b.bit&(b.bit-1) != 0 || b.bit <= prev {
			t.Errorf("schema option %#x: not a single bit above %#x", uint64(b.bit), uint64(prev))
		}
		prev = b.bit
	}
}

// Azure SQL Database has no replication catalog: each entry point refuses
// before any statement is sent (this Server has no connection to send one).
func TestReplicationRefusedOnAzureSQLDatabase(t *testing.T) {
	s := &Server{info: &ServerInfo{EngineEdition: int(EngineAzureSQLDatabase)}}
	d := s.DatabaseRef("db")
	ctx := context.Background()
	calls := map[string]func() error{
		"ReplicationInfo":             func() error { _, err := s.ReplicationInfo(ctx); return err },
		"LocalPublications":           func() error { _, err := s.LocalPublications(ctx); return err },
		"LocalSubscriptions":          func() error { _, err := s.LocalSubscriptions(ctx); return err },
		"Database.Publications":       func() error { _, err := d.Publications(ctx); return err },
		"Database.LocalSubscriptions": func() error { _, err := d.LocalSubscriptions(ctx); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("%s: err = %v, want ErrUnsupportedVersion", name, err)
		}
	}
}

func TestReplicationEnumStrings(t *testing.T) {
	for v, want := range map[PublicationType]string{
		PublicationTransactional: "Transactional", PublicationSnapshot: "Snapshot",
		PublicationPeerToPeer: "Peer-to-peer", PublicationMerge: "Merge", 9: "PublicationType(9)",
	} {
		if got := v.String(); got != want {
			t.Errorf("PublicationType(%d) = %q, want %q", int(v), got, want)
		}
	}
	for v, want := range map[SubscriptionType]string{
		SubscriptionPush: "Push", SubscriptionPull: "Pull", SubscriptionAnonymous: "Anonymous",
	} {
		if got := v.String(); got != want {
			t.Errorf("SubscriptionType(%d) = %q, want %q", int(v), got, want)
		}
	}
}

func TestMonitorWarningWarnings(t *testing.T) {
	got := MonitorWarning(2 | 32).Warnings()
	want := []string{"Latency exceeds the threshold", "Merge run duration exceeds the threshold (slow link)"}
	if !slices.Equal(got, want) {
		t.Errorf("Warnings() = %q, want %q", got, want)
	}
	if got := MonitorWarning(128 | 1).Warnings(); len(got) != 2 || got[1] != "warning 128" {
		t.Errorf("undocumented bit: Warnings() = %q", got)
	}
	if got := MonitorWarning(0).Warnings(); got != nil {
		t.Errorf("zero: Warnings() = %q, want none", got)
	}
}

// The status codes are sqlrepl.h's, shared by the history tables and the
// monitor procedures; pinned by name so a reordered const block fails here.
func TestReplAgentStatusCodes(t *testing.T) {
	for code, want := range map[int]string{
		0: "Never run", 1: "Started", 2: "Succeeded", 3: "In progress", 4: "Idle", 5: "Retrying", 6: "Failed",
		7: "ReplAgentStatus(7)",
	} {
		if got := ReplAgentStatus(code).String(); got != want {
			t.Errorf("ReplAgentStatus(%d) = %q, want %q", code, got, want)
		}
	}
	for k, want := range map[ReplAgentKind]string{
		ReplSnapshotAgent: "Snapshot Agent", ReplLogReaderAgent: "Log Reader Agent",
		ReplDistributionAgent: "Distribution Agent", ReplMergeAgent: "Merge Agent",
	} {
		if got := k.String(); got != want {
			t.Errorf("ReplAgentKind(%d) = %q, want %q", int(k), got, want)
		}
		if replAgentProc(k) == "" {
			t.Errorf("%v has no sp_MSenum procedure", k)
		}
	}
	for code, want := range map[int]PublicationType{0: PublicationTransactional, 1: PublicationSnapshot, 2: PublicationMerge} {
		if got := monitorPublicationType(code); got != want {
			t.Errorf("monitorPublicationType(%d) = %v, want %v", code, got, want)
		}
	}
}

// sp_MSenum_* return times as sys.fn_replformatdatetime text.
func TestParseReplTime(t *testing.T) {
	got := parseReplTime("20261006 23:52:38.673")
	want := time.Date(2026, 10, 6, 23, 52, 38, 673_000_000, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("parseReplTime = %v, want %v", got, want)
	}
	if !parseReplTime("").IsZero() || !parseReplTime("garbage").IsZero() {
		t.Error("unparseable text should read as the zero time")
	}
	if s := want.Format(replTimeLayout); s != "20261006 23:52:38.673" {
		t.Errorf("format = %q", s)
	}
}

func TestMonitorPublishersRefusedOnAzureSQLDatabase(t *testing.T) {
	s := &Server{info: &ServerInfo{EngineEdition: int(EngineAzureSQLDatabase)}}
	if _, err := s.MonitorPublishers(context.Background()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("MonitorPublishers: err = %v, want ErrUnsupportedVersion", err)
	}
}
