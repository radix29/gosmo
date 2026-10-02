//go:build livedb

// Live verification of the 2026-10-02 handle-centric writes (gossms review
// plan W18, T42): role and server-role membership, file and filegroup writes,
// the category drop, AddStep/InsertStep reading their step back, JobStep.Drop
// and Alert.RemoveNotification. Each write goes through its handle — the Ref
// form where a caller would have nothing read — and the catalog is read back.
//
//	go test -tags livedb . -run TestLiveHandleWrites -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database, login, server role, job,
// alert, operator and category; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLiveHandleWrites(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_handle_writes_live")
	t.Cleanup(drop)
	srv := d.Server()

	t.Run("database role membership", func(t *testing.T) {
		for _, stmt := range []string{"CREATE USER [hw'u] WITHOUT LOGIN", "CREATE ROLE [hw]]r]"} {
			if _, err := d.exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		members := func() []string {
			ms, err := d.RoleMembers(ctx, "hw]r")
			if err != nil {
				t.Fatalf("RoleMembers: %v", err)
			}
			var out []string
			for _, m := range ms {
				out = append(out, m.Name)
			}
			return out
		}
		r := d.RoleRef("hw]r")
		if err := r.AddMember(ctx, "hw'u"); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		if got := members(); len(got) != 1 || got[0] != "hw'u" {
			t.Fatalf("members after AddMember = %q, want [hw'u]", got)
		}
		if err := r.RemoveMember(ctx, "hw'u"); err != nil {
			t.Fatalf("RemoveMember: %v", err)
		}
		if got := members(); len(got) != 0 {
			t.Fatalf("members after RemoveMember = %q, want none", got)
		}
	})

	t.Run("server role membership", func(t *testing.T) {
		const login, role = "gosmo_hw'login", "gosmo_hw]srole"
		cleanup := func() {
			c := context.Background()
			db.ExecContext(c, "IF SUSER_ID(N'gosmo_hw]srole') IS NOT NULL DROP SERVER ROLE [gosmo_hw]]srole]")
			db.ExecContext(c, "IF SUSER_ID(N'gosmo_hw''login') IS NOT NULL DROP LOGIN [gosmo_hw'login]")
		}
		cleanup()
		t.Cleanup(cleanup)
		if _, err := srv.CreateLogin(ctx, CreateLoginRequest{Name: login, Password: "Hw!" + strings.Repeat("x9", 8)}); err != nil {
			t.Fatalf("CreateLogin: %v", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE SERVER ROLE [gosmo_hw]]srole]"); err != nil {
			t.Fatalf("CREATE SERVER ROLE: %v", err)
		}
		r := srv.ServerRoleRef(role)
		if err := r.AddMember(ctx, login); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		ms, err := srv.ServerRoleMembers(ctx, role)
		if err != nil || len(ms) != 1 || ms[0].Name != login {
			t.Fatalf("members after AddMember = %v, %v; want [%s]", ms, err, login)
		}
		if err := r.RemoveMember(ctx, login); err != nil {
			t.Fatalf("RemoveMember: %v", err)
		}
		if ms, err := srv.ServerRoleMembers(ctx, role); err != nil || len(ms) != 0 {
			t.Fatalf("members after RemoveMember = %v, %v; want none", ms, err)
		}
	})

	t.Run("files and filegroups", func(t *testing.T) {
		files, err := d.Files(ctx)
		if err != nil || len(files) == 0 {
			t.Fatalf("Files: %v, %v", files, err)
		}
		if files[0].Database() != d {
			t.Fatalf("Files()[0].Database() = %p, want the database it was read from", files[0].Database())
		}
		// The server's path separator, not the client's: the file lands on
		// the server, which may be Windows while the test runs on Linux.
		primary := files[0].PhysicalName
		sep := "\\"
		if strings.HasPrefix(primary, "/") {
			sep = "/"
		}
		dir := primary[:strings.LastIndex(primary, sep)+1]

		if err := d.AddFileGroup(ctx, "HW'FG"); err != nil {
			t.Fatalf("AddFileGroup: %v", err)
		}
		if err := d.AddFile(ctx, DatabaseFileSpec{Name: "hw_dat", FileGroup: "HW'FG",
			Path: dir + "gosmo_handle_writes_live_hw.ndf", SizeKB: 8192}); err != nil {
			t.Fatalf("AddFile: %v", err)
		}

		fileNamed := func(name string) *DatabaseFileInfo {
			fs, err := d.Files(ctx)
			if err != nil {
				t.Fatalf("Files: %v", err)
			}
			for _, f := range fs {
				if f.Name == name {
					return f
				}
			}
			return nil
		}
		group := func() *FileGroup {
			fgs, err := d.FileGroups(ctx)
			if err != nil {
				t.Fatalf("FileGroups: %v", err)
			}
			for _, fg := range fgs {
				if fg.Name == "HW'FG" {
					return fg
				}
			}
			return nil
		}

		f := d.FileRef("hw_dat")
		if err := f.Alter(ctx, FileModify{NewName: "hw'dat2", SizeKB: 16384}); err != nil {
			t.Fatalf("Alter: %v", err)
		}
		if f.Name != "hw'dat2" {
			t.Errorf("handle after a renaming Alter is named %q, want hw'dat2", f.Name)
		}
		if got := fileNamed("hw'dat2"); got == nil || got.SizeKB != 16384 || got.FileGroup != "HW'FG" {
			t.Fatalf("file after Alter = %+v, want hw'dat2 at 16384 KB in HW'FG", got)
		}

		fg := group()
		if fg == nil || fg.Database() != d {
			t.Fatalf("FileGroups did not return HW'FG wired to its database: %+v", fg)
		}
		if err := fg.SetReadOnly(ctx, true, TerminationRollbackImmediate); err != nil {
			t.Fatalf("SetReadOnly(true): %v", err)
		}
		if got := group(); !fg.IsReadOnly || got == nil || !got.IsReadOnly {
			t.Fatalf("after SetReadOnly(true): handle %v, catalog %+v", fg.IsReadOnly, got)
		}
		if err := fg.SetReadOnly(ctx, false, TerminationRollbackImmediate); err != nil {
			t.Fatalf("SetReadOnly(false): %v", err)
		}
		if err := d.FileGroupRef("HW'FG").SetDefault(ctx); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
		if got := group(); got == nil || !got.IsDefault || got.IsReadOnly {
			t.Fatalf("after SetDefault: %+v, want default and read-write", got)
		}
		// A default filegroup cannot be removed; hand the role back first.
		if err := d.FileGroupRef("PRIMARY").SetDefault(ctx); err != nil {
			t.Fatalf("PRIMARY SetDefault: %v", err)
		}

		if err := d.FileRef("hw'dat2").Drop(ctx); err != nil {
			t.Fatalf("file Drop: %v", err)
		}
		if got := fileNamed("hw'dat2"); got != nil {
			t.Fatalf("file still there after Drop: %+v", got)
		}
		if err := d.FileGroupRef("HW'FG").Drop(ctx); err != nil {
			t.Fatalf("filegroup Drop: %v", err)
		}
		if got := group(); got != nil {
			t.Fatalf("filegroup still there after Drop: %+v", got)
		}
	})

	t.Run("category drop", func(t *testing.T) {
		const name = "gosmo_hw'cat"
		srv.CategoryRef(CategoryClassAlert, name).Drop(context.Background())
		if _, err := srv.CreateCategory(ctx, CreateCategoryRequest{Class: CategoryClassAlert, Name: name}); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		c, err := srv.CategoryByName(ctx, CategoryClassAlert, name)
		if err != nil || c.Server() != srv || c.ID == 0 {
			t.Fatalf("CategoryByName = %+v, %v", c, err)
		}
		if err := c.Drop(ctx); err != nil {
			t.Fatalf("Drop: %v", err)
		}
		if _, err := srv.CategoryByName(ctx, CategoryClassAlert, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("CategoryByName after Drop: %v, want ErrNotFound", err)
		}
	})

	t.Run("job steps", func(t *testing.T) {
		const job = "gosmo_hw'job"
		cleanup := func() { srv.JobRef(job).Drop(context.Background()) }
		cleanup()
		t.Cleanup(cleanup)
		if _, err := srv.CreateJob(ctx, CreateJobRequest{Name: job}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		j := srv.JobRef(job) // a Ref on purpose: the read-back matches by job name
		a, err := j.AddStep(ctx, JobStepRequest{Name: "a'1", Subsystem: "TSQL", Command: "SELECT 1", OnSuccessAction: 1, OnFailAction: 2})
		if err != nil {
			t.Fatalf("AddStep: %v", err)
		}
		if a.StepID != 1 || a.Name != "a'1" || a.Command != "SELECT 1" {
			t.Fatalf("AddStep returned %+v, want step 1 a'1 read back", a)
		}
		b, err := j.InsertStep(ctx, JobStepRequest{Name: "b", Subsystem: "TSQL", Command: "SELECT 2", OnSuccessAction: 3, OnFailAction: 2}, 1)
		if err != nil {
			t.Fatalf("InsertStep: %v", err)
		}
		if b.StepID != 1 || b.Name != "b" {
			t.Fatalf("InsertStep returned %+v, want step 1 b read back", b)
		}
		// b is step 1 now and a was pushed to 2; drop b through the handle
		// InsertStep returned.
		if err := b.Drop(ctx); err != nil {
			t.Fatalf("Drop: %v", err)
		}
		full, err := srv.JobByName(ctx, job)
		if err != nil {
			t.Fatalf("JobByName: %v", err)
		}
		steps, err := full.Steps(ctx)
		if err != nil || len(steps) != 1 || steps[0].Name != "a'1" || steps[0].StepID != 1 {
			t.Fatalf("steps after Drop = %v, %v; want a'1 alone at 1", stepNames(steps), err)
		}
	})

	t.Run("alert notification", func(t *testing.T) {
		const alert, op = "gosmo_hw'alert", "gosmo_hw'op"
		cleanup := func() {
			c := context.Background()
			srv.AlertRef(alert).Drop(c)
			srv.OperatorRef(op).Drop(c)
		}
		cleanup()
		t.Cleanup(cleanup)
		if _, err := srv.CreateOperator(ctx, CreateOperatorRequest{Name: op, Enabled: true, EmailAddress: "hw@example.invalid"}); err != nil {
			t.Fatalf("CreateOperator: %v", err)
		}
		a, err := srv.CreateAlert(ctx, CreateAlertRequest{Name: alert, Enabled: true, Severity: 25})
		if err != nil {
			t.Fatalf("CreateAlert: %v", err)
		}
		if err := a.Notify(ctx, op, NotifyMethodEmail); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		if ns, err := a.Notifications(ctx); err != nil || len(ns) != 1 {
			t.Fatalf("Notifications after Notify = %v, %v; want one", ns, err)
		}
		if err := a.RemoveNotification(ctx, op); err != nil {
			t.Fatalf("RemoveNotification: %v", err)
		}
		if ns, err := a.Notifications(ctx); err != nil || len(ns) != 0 {
			t.Fatalf("Notifications after RemoveNotification = %v, %v; want none", ns, err)
		}
	})
}
