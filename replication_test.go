package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"
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
