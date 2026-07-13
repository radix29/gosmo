package gosmo

import "testing"

func TestNormalizeFileGrowth(t *testing.T) {
	cases := []struct {
		name                    string
		maxSizePages, growthRaw int64
		isPercentGrowth         bool
		wantMaxKB, wantGrowthKB int64
		wantGrowthPercent       int
	}{
		{
			name:         "unlimited max size, KB growth",
			maxSizePages: -1, growthRaw: 1024, isPercentGrowth: false,
			wantMaxKB: -1, wantGrowthKB: 8192, wantGrowthPercent: 0,
		},
		{
			name:         "bounded max size, percent growth",
			maxSizePages: 4096, growthRaw: 10, isPercentGrowth: true,
			wantMaxKB: 32768, wantGrowthKB: 0, wantGrowthPercent: 10,
		},
		{
			name:         "zero max size (log file with no explicit cap) stays zero, not unlimited",
			maxSizePages: 0, growthRaw: 512, isPercentGrowth: false,
			wantMaxKB: 0, wantGrowthKB: 4096, wantGrowthPercent: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMax, gotGrowth, gotPct := normalizeFileGrowth(c.maxSizePages, c.growthRaw, c.isPercentGrowth)
			if gotMax != c.wantMaxKB {
				t.Errorf("maxSizeKB = %d, want %d", gotMax, c.wantMaxKB)
			}
			if gotGrowth != c.wantGrowthKB {
				t.Errorf("growthKB = %d, want %d", gotGrowth, c.wantGrowthKB)
			}
			if gotPct != c.wantGrowthPercent {
				t.Errorf("growthPercent = %d, want %d", gotPct, c.wantGrowthPercent)
			}
		})
	}
}
