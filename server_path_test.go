package gosmo

import (
	"strings"
	"testing"
)

func TestJoinServerPath(t *testing.T) {
	for _, tt := range []struct{ dir, file, want string }{
		{`C:\Data`, "a.mdf", `C:\Data\a.mdf`},
		{`C:\Data\`, "a.mdf", `C:\Data\a.mdf`},
		{`C:\`, "a.mdf", `C:\a.mdf`},
		{"/var/opt/mssql/data", "a.mdf", "/var/opt/mssql/data/a.mdf"},
		{"/var/opt/mssql/data/", "a.mdf", "/var/opt/mssql/data/a.mdf"},
		{"/", "a.mdf", "/a.mdf"},
		{`\\host\share\`, "a.bak", `\\host\share\a.bak`},
		{`\\host\share`, "a.bak", `\\host\share\a.bak`},
		{"", "a.mdf", "a.mdf"},
		{`C:\Backup`, "", `C:\Backup\`},
		{`C:\Backup\`, "", `C:\Backup\`},
		// One trailing separator is kept, not normalised: the dir is the
		// caller's, and a doubled one is the caller's to fix.
		{`C:\Backup\\`, "a.bak", `C:\Backup\\a.bak`},
	} {
		if got := JoinServerPath(tt.dir, tt.file); got != tt.want {
			t.Errorf("JoinServerPath(%q, %q) = %q, want %q", tt.dir, tt.file, got, tt.want)
		}
	}
}

func TestServerPathParts(t *testing.T) {
	for _, tt := range []struct{ path, dir, base, ext string }{
		{`C:\Data\a.mdf`, `C:\Data`, "a.mdf", ".mdf"},
		{`C:\a.bak`, `C:\`, "a.bak", ".bak"},
		{"/var/opt/mssql/data/a.ldf", "/var/opt/mssql/data", "a.ldf", ".ldf"},
		{"/a.bak", "/", "a.bak", ".bak"},
		{`\\host\share\a.bak`, `\\host\share`, "a.bak", ".bak"},
		{`C:\Data/mixed.ndf`, `C:\Data`, "mixed.ndf", ".ndf"},
		{"a.mdf", "", "a.mdf", ".mdf"},
		{`C:\Data\fs_container`, `C:\Data`, "fs_container", ""},
		{`C:\My.Data\noext`, `C:\My.Data`, "noext", ""},
		{`C:\Data\.ldf`, `C:\Data`, ".ldf", ""},
		{`C:\Data\`, `C:\Data`, "", ""},
		{"", "", "", ""},
	} {
		if got := ServerPathDir(tt.path); got != tt.dir {
			t.Errorf("ServerPathDir(%q) = %q, want %q", tt.path, got, tt.dir)
		}
		if got := ServerPathBase(tt.path); got != tt.base {
			t.Errorf("ServerPathBase(%q) = %q, want %q", tt.path, got, tt.base)
		}
		if got := ServerPathExt(tt.path); got != tt.ext {
			t.Errorf("ServerPathExt(%q) = %q, want %q", tt.path, got, tt.ext)
		}
		// A mixed-separator path joins back with the directory's own one.
		if tt.dir != "" && tt.base != "" && !strings.Contains(tt.path, "/mixed") {
			if got := JoinServerPath(ServerPathDir(tt.path), ServerPathBase(tt.path)); got != tt.path {
				t.Errorf("JoinServerPath(Dir, Base) of %q = %q, want it back", tt.path, got)
			}
		}
	}
}
