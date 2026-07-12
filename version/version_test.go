package version

import "testing"

func TestParsePseudoVersion(t *testing.T) {
	tests := []struct {
		name       string
		v          string
		wantCommit string
		wantDate   string
		wantOK     bool
	}{
		{
			name:       "pseudo-version, no prior tag",
			v:          "v0.0.0-20191109021931-daa7c04131f5",
			wantCommit: "daa7c04131f5",
			wantDate:   "2019-11-09T02:19:31Z",
			wantOK:     true,
		},
		{
			name:       "pseudo-version following a pre-release tag",
			v:          "v1.2.3-0.20220101000000-abcdefabcdef",
			wantCommit: "abcdefabcdef",
			wantDate:   "2022-01-01T00:00:00Z",
			wantOK:     true,
		},
		{
			name:       "pseudo-version with +incompatible suffix",
			v:          "v2.0.0-20220101000000-abcdefabcdef+incompatible",
			wantCommit: "abcdefabcdef",
			wantDate:   "2022-01-01T00:00:00Z",
			wantOK:     true,
		},
		{
			name:   "plain semver tag",
			v:      "v0.0.3",
			wantOK: false,
		},
		{
			name:   "local filesystem replace",
			v:      "(devel)",
			wantOK: false,
		},
		{
			name:   "empty version",
			v:      "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commit, date, ok := parsePseudoVersion(tt.v)
			if ok != tt.wantOK {
				t.Fatalf("parsePseudoVersion(%q) ok = %v, want %v", tt.v, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if commit != tt.wantCommit {
				t.Errorf("parsePseudoVersion(%q) commit = %q, want %q", tt.v, commit, tt.wantCommit)
			}
			if date != tt.wantDate {
				t.Errorf("parsePseudoVersion(%q) date = %q, want %q", tt.v, date, tt.wantDate)
			}
		})
	}
}
