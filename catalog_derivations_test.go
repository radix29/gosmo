package gosmo

import (
	"testing"
	"time"
)

// The derivations below gate drops and editors in a caller, so each one's
// boundary is pinned.

// sa is the deliberate hole: DROP LOGIN sa is refused, but ALTER LOGIN sa WITH
// NAME succeeds and renaming it is a documented hardening step. The ## logins
// are system even though the server permits dropping them.
func TestLoginIsSystemMatchesOnlyTheHashNames(t *testing.T) {
	for _, name := range []string{"##MS_PolicyEventProcessingLogin##", "##MS_PolicyTsqlExecutionLogin##"} {
		if !(&Login{Name: name}).IsSystem() {
			t.Errorf("%s: IsSystem = false", name)
		}
	}
	// sa and the service logins stay editable: renaming sa is hardening, and
	// the service logins are ordinary Windows logins.
	for _, name := range []string{"sa", `NT SERVICE\SQLSERVERAGENT`, `NT AUTHORITY\SYSTEM`, "app#login"} {
		if (&Login{Name: name}).IsSystem() {
			t.Errorf("%s: IsSystem = true", name)
		}
	}
}

func TestLoginIsSQLLogin(t *testing.T) {
	if !(&Login{LoginType: "SQL_LOGIN"}).IsSQLLogin() || (&Login{LoginType: "WINDOWS_LOGIN"}).IsSQLLogin() {
		t.Error("IsSQLLogin must be true for SQL_LOGIN alone")
	}
}

func TestJobIsSystemIsTheSyspolicyPrefix(t *testing.T) {
	if !(&Job{Name: "syspolicy_purge_history"}).IsSystem() {
		t.Error("syspolicy_purge_history: IsSystem = false")
	}
	for _, name := range []string{"sysutility_get_views_data_into_cache_tables", "mdw_purge_data_[x]", "SSIS Server Maintenance Job", "nightly"} {
		if (&Job{Name: name}).IsSystem() {
			t.Errorf("%s: IsSystem = true", name)
		}
	}
}

func TestUserIsMappedAndIsExternal(t *testing.T) {
	for _, tt := range []struct {
		u                  User
		mapped, isExternal bool
	}{
		{User{UserType: "CERTIFICATE_MAPPED_USER"}, true, false},
		{User{UserType: "ASYMMETRIC_KEY_MAPPED_USER"}, true, false},
		{User{UserType: "EXTERNAL_USER"}, false, true},
		{User{UserType: "EXTERNAL_GROUPS"}, false, true},
		{User{UserType: "SQL_USER", AuthType: "EXTERNAL"}, false, true},
		{User{UserType: "SQL_USER", AuthType: "INSTANCE"}, false, false},
		{User{UserType: "WINDOWS_USER", AuthType: "WINDOWS"}, false, false},
	} {
		if got := tt.u.IsMapped(); got != tt.mapped {
			t.Errorf("%s/%s: IsMapped = %v", tt.u.UserType, tt.u.AuthType, got)
		}
		if got := tt.u.IsExternal(); got != tt.isExternal {
			t.Errorf("%s/%s: IsExternal = %v", tt.u.UserType, tt.u.AuthType, got)
		}
	}
}

func TestCertificateIsExpired(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if !(&Certificate{ExpiryDate: now.Add(-time.Second)}).IsExpired(now) {
		t.Error("a certificate past its expiry date is not expired")
	}
	if (&Certificate{ExpiryDate: now.Add(time.Hour)}).IsExpired(now) {
		t.Error("a certificate before its expiry date is expired")
	}
	if (&Certificate{}).IsExpired(now) {
		t.Error("a certificate with no expiry date read is expired")
	}
}

// TestServerInfoIsWindows pins all three branches. Getting this wrong is not a
// cosmetic slip in a caller: it picks the path rules a server-side file browse
// runs on, and POSIX rules over a Windows host make every "C:\..." path
// unsplittable — BACKUP is then handed a destination the server cannot write.
func TestServerInfoIsWindows(t *testing.T) {
	for _, tc := range []struct {
		what     string
		platform string
		backup   string
		want     bool
	}{
		// Platform is the direct answer and outranks the path, which is the
		// only ordering that survives a Linux instance whose backup directory
		// happens to contain a backslash.
		{"reported Windows", "Windows", `C:\Backup`, true},
		{"reported Linux", "Linux", "/var/opt/mssql/data", false},
		{"reported Windows despite a posix path", "Windows", "/var/opt/mssql", true},
		{"reported Linux despite a backslash", "Linux", `/var/opt/odd\name`, false},
		// Unreported: fall back to reading the default backup path.
		{"unreported, windows path", "", `C:\Program Files\Backup`, true},
		{"unreported, posix path", "", "/var/opt/mssql/data", false},
		// Azure names no host OS, so the path decides there too.
		{"Azure, windows path", "Azure", `C:\Backup`, true},
		// Nothing to go on at all. POSIX is the safer guess: its rules leave a
		// Windows path in one piece as a single name, where Windows rules
		// applied to a posix path split it at separators that aren't there.
		{"nothing reported", "", "", false},
		// An unrecognized platform string is not a third answer — it falls
		// through to the path like an empty one.
		{"unknown platform, windows path", "Darwin", `D:\Backup`, true},
	} {
		info := &ServerInfo{Platform: tc.platform, DefaultBackupPath: tc.backup}
		if got := info.IsWindows(); got != tc.want {
			t.Errorf("%s: IsWindows(%q, %q) = %v, want %v", tc.what, tc.platform, tc.backup, got, tc.want)
		}
	}
	if (*ServerInfo)(nil).IsWindows() {
		t.Error("a nil ServerInfo is Windows")
	}
}

// TestQueryStoreInfoIsReadable. READ_ONLY is what a Query Store that filled its
// quota degrades to: it stopped collecting, but everything it already holds is
// still worth reporting. Treating it as off would blank every report at exactly
// the moment they matter.
func TestQueryStoreInfoIsReadable(t *testing.T) {
	for state, want := range map[QueryStoreState]bool{
		QueryStoreReadWrite: true, QueryStoreReadOnly: true,
		QueryStoreOff: false, QueryStoreError: false, "": false,
	} {
		if got := (&QueryStoreInfo{ActualState: state}).IsReadable(); got != want {
			t.Errorf("%q: IsReadable = %v, want %v", state, got, want)
		}
	}
}
