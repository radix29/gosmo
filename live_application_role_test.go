//go:build livedb

// Live round trip of the application-role family: create, read by name and
// in the listing, rename, change the default schema and the password, drop.
// The password change is proved by sp_setapprole accepting the new one and
// refusing the old.
//
//	go test -tags livedb . -run TestLiveApplicationRole -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

func TestLiveApplicationRoleRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_approle_live")
	t.Cleanup(drop)

	if _, err := d.exec(ctx, "CREATE SCHEMA [ap'p]"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	pw1, pw2 := "Ar1!"+strings.Repeat("x9", 8), "Ar2'"+strings.Repeat("y8", 8)

	r, err := d.CreateApplicationRole(ctx, CreateApplicationRoleRequest{Name: "ar]1", Password: pw1, DefaultSchema: "ap'p"})
	if err != nil {
		t.Fatalf("CreateApplicationRole: %v", err)
	}
	if r.Name != "ar]1" || r.DefaultSchema != "ap'p" || r.PrincipalID == 0 || r.CreateDate.IsZero() || r.ModifyDate.IsZero() {
		t.Fatalf("created = %+v, want every field read back", r)
	}

	list, err := d.ApplicationRoles(ctx)
	if err != nil {
		t.Fatalf("ApplicationRoles: %v", err)
	}
	if len(list) != 1 || *list[0] != *r {
		t.Fatalf("ApplicationRoles = %v, want exactly %+v — the listing and the by-name read share one select", list, r)
	}

	// The family must stay out of the other principal listings, as they stay
	// out of it.
	if roles, err := d.Roles(ctx); err != nil {
		t.Fatalf("Roles: %v", err)
	} else {
		for _, x := range roles {
			if x.Name == "ar]1" {
				t.Errorf("Roles lists the application role")
			}
		}
	}

	setAppRole := func(pw string) error {
		// sp_setapprole without a cookie cannot be undone, so it runs on a
		// connection of its own that is discarded afterwards.
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		defer func() {
			// ErrBadConn makes database/sql discard the connection rather
			// than return it to the pool with the role still set.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			_ = conn.Close()
		}()
		// Literals, not parameters: a parameterised batch runs through
		// sp_executesql, and sp_setapprole refuses anything but the ad hoc
		// level (Msg 15422).
		_, err = conn.ExecContext(ctx, "USE [gosmo_approle_live]; EXEC sys.sp_setapprole @rolename = "+
			QuoteLiteral("ar]1")+", @password = "+QuoteLiteral(pw))
		return err
	}
	if err := setAppRole(pw1); err != nil {
		t.Fatalf("sp_setapprole with the create password: %v", err)
	}

	h := d.ApplicationRoleRef("ar]1")
	if err := h.ChangePassword(ctx, pw2); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if err := setAppRole(pw2); err != nil {
		t.Errorf("sp_setapprole with the new password: %v", err)
	}
	if err := setAppRole(pw1); err == nil {
		t.Errorf("sp_setapprole still takes the old password")
	}

	if err := h.SetDefaultSchema(ctx, "dbo"); err != nil {
		t.Fatalf("SetDefaultSchema: %v", err)
	}
	if h.DefaultSchema != "dbo" {
		t.Errorf("receiver DefaultSchema = %q, want dbo", h.DefaultSchema)
	}
	if err := h.Rename(ctx, "ar'2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if h.Name != "ar'2" {
		t.Errorf("receiver Name = %q, want ar'2", h.Name)
	}
	got, err := d.ApplicationRoleByName(ctx, "ar'2")
	if err != nil {
		t.Fatalf("ApplicationRoleByName after rename: %v", err)
	}
	if got.DefaultSchema != "dbo" || got.PrincipalID != r.PrincipalID {
		t.Errorf("after rename = %+v, want default schema dbo and principal %d", got, r.PrincipalID)
	}
	if _, err := d.ApplicationRoleByName(ctx, "ar]1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ApplicationRoleByName(old name) = %v, want ErrNotFound", err)
	}

	if err := h.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if list, err := d.ApplicationRoles(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("ApplicationRoles after Drop = %v, %v; want none", list, err)
	}
}
