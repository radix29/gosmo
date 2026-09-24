//go:build livedb

// Live verification of the credential read/write path: that CREATE, ALTER and
// DROP CREDENTIAL as gosmo builds them are accepted, that the row reads back
// through both Credentials and CredentialByName with the same
// values, and that a generated script recreates the same credential.
//
// The unit tests pin the statement text; only a live run settles what SQL
// Server accepts — the ALTER's secret semantics in particular, where omitting
// SECRET clears the stored secret rather than preserving it.
//
//	go test -tags livedb . -run TestLiveCredential -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway credential; touches nothing else.
package gosmo

import (
	"errors"
	"strings"
	"testing"
)

const liveCredentialName = "gossms_plan_cred"

func TestLiveCredentialCreateAlterReadDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	cleanup := func() {
		if c, err := s.CredentialByName(ctx, liveCredentialName); err == nil {
			if err := c.Drop(ctx); err != nil {
				t.Logf("cleanup of credential %q: %v", liveCredentialName, err)
			}
		}
	}
	cleanup()
	defer cleanup()

	c, err := s.CreateCredential(ctx, CreateCredentialRequest{
		Name:     liveCredentialName,
		Identity: `GOSMO\svc_account`,
		Secret:   "gosmo-live-secret-1",
	})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if c.Identity != `GOSMO\svc_account` {
		t.Errorf("created credential reads back identity %q, want %q", c.Identity, `GOSMO\svc_account`)
	}
	if c.CredentialID == 0 {
		t.Error("created credential has no credential_id — the read-back did not happen")
	}
	if c.TargetType != "" {
		t.Errorf("an ordinary credential reports target_type %q, want empty", c.TargetType)
	}

	// The listing and the by-name read must agree; the by-name read is what
	// every Properties page opens with.
	var listed *Credential
	all, err := s.Credentials(ctx)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	for _, got := range all {
		if got.Name == liveCredentialName {
			listed = got
		}
	}
	if listed == nil {
		t.Fatalf("the new credential is not in Credentials's %d rows", len(all))
	}
	if listed.Identity != c.Identity || listed.CredentialID != c.CredentialID {
		t.Errorf("listing has %+v, by-name read has %+v", listed, c)
	}

	// ALTER with a new secret, then ALTER without one. Both must be accepted;
	// the second clears the stored secret, which is why the API takes a
	// pointer rather than a string.
	newSecret := "gosmo-live-secret-2"
	if err := c.Alter(ctx, `GOSMO\other_account`, &newSecret); err != nil {
		t.Fatalf("Alter with a secret: %v", err)
	}
	if c.Identity != `GOSMO\other_account` {
		t.Errorf("Alter did not mirror the identity: %q", c.Identity)
	}
	after, err := s.CredentialByName(ctx, liveCredentialName)
	if err != nil {
		t.Fatalf("re-read after alter: %v", err)
	}
	if after.Identity != `GOSMO\other_account` {
		t.Errorf("server has identity %q after the alter, want %q", after.Identity, `GOSMO\other_account`)
	}
	if err := c.Alter(ctx, `GOSMO\third_account`, nil); err != nil {
		t.Fatalf("Alter without a secret: %v", err)
	}

	// The script must carry an obvious placeholder, not a silently absent
	// SECRET clause.
	script, err := NewServerScripter(s, ScriptOptions{}).ScriptCredential(ctx, liveCredentialName)
	if err != nil {
		t.Fatalf("ScriptCredential: %v", err)
	}
	if !strings.Contains(script, credentialSecretPlaceholder) {
		t.Errorf("script carries no secret placeholder:\n%s", script)
	}
	if !strings.Contains(script, `GOSMO\third_account`) {
		t.Errorf("script does not carry the current identity:\n%s", script)
	}

	if err := c.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := s.CredentialByName(ctx, liveCredentialName); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the drop, the by-name read returned %v, want ErrNotFound", err)
	}
}

// The generated CREATE script has to be something SQL Server actually accepts,
// placeholder and all — a script nobody can run is not a script.
func TestLiveCredentialScriptRunsAsGenerated(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const name = liveCredentialName + "_script"
	drop := func() {
		if c, err := s.CredentialByName(ctx, name); err == nil {
			c.Drop(ctx)
		}
	}
	drop()
	defer drop()

	src, err := s.CreateCredential(ctx, CreateCredentialRequest{Name: name, Identity: "scripted_identity"})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	script := buildCredentialScript(src, ScriptOptions{})
	if err := src.Drop(ctx); err != nil {
		t.Fatalf("Drop before replay: %v", err)
	}

	for _, batch := range strings.Split(script, "\nGO\n") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying the generated script failed: %v\n%s", err, batch)
		}
	}
	back, err := s.CredentialByName(ctx, name)
	if err != nil {
		t.Fatalf("read back the scripted credential: %v", err)
	}
	if back.Identity != "scripted_identity" {
		t.Errorf("scripted credential has identity %q, want %q", back.Identity, "scripted_identity")
	}
}
