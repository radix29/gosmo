package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Under WithScript InTransaction only runs fn: the statements are collected,
// with no BEGIN TRANSACTION or COMMIT around them, and no connection is used —
// the Server here has none.
func TestInTransactionUnderScriptOnlyRunsFn(t *testing.T) {
	s := &Server{}
	ctx, col := WithScript(context.Background())
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		_, err := s.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: "p"})
		return err
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	if got, want := col.Statements(), []string{"CREATE RESOURCE POOL [p]"}; !slices.Equal(got, want) {
		t.Errorf("captured %q, want %q", got, want)
	}
}

// One context carries one transaction, so a second Server's InTransaction
// inside the first refuses, and joining the same Server's does not begin
// another.
func TestInTransactionNesting(t *testing.T) {
	a, b := &Server{}, &Server{}
	ctx := context.WithValue(context.Background(), txCtxKey{}, &serverTx{server: a})

	ran := false
	if err := a.InTransaction(ctx, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("nested InTransaction on the same Server: ran %v, err %v — want it to join", ran, err)
	}
	if err := b.InTransaction(ctx, func(context.Context) error { return nil }); !errors.Is(err, ErrUnsupported) {
		t.Errorf("InTransaction on another Server inside one: %v, want ErrUnsupported", err)
	}
	if txFrom(ctx, b) != nil {
		t.Error("txFrom matched a transaction on another Server")
	}
}

// The methods that need a session of their own refuse inside a transaction
// before touching the server.
func TestInTransactionRefusesOwnSessionMethods(t *testing.T) {
	s := &Server{}
	ctx := context.WithValue(context.Background(), txCtxKey{}, &serverTx{server: s})
	d := s.DatabaseRef("db")
	if _, err := d.BulkInsert(ctx, BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"a"}}, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BulkInsert: %v, want ErrUnsupported", err)
	}
	if _, err := s.EffectiveServerPermissions(ctx, "l"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("EffectiveServerPermissions: %v, want ErrUnsupported", err)
	}
	if _, err := d.EffectivePermissions(ctx, "u"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("EffectivePermissions: %v, want ErrUnsupported", err)
	}
	progress := func(int, string) {}
	if err := s.Backup(ctx, BackupOptions{Database: "db", Devices: []BackupTarget{DiskTarget("x.bak")}, Progress: progress}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Backup with progress: %v, want ErrUnsupported", err)
	}
}
