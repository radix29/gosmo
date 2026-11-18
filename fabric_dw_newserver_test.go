package gosmo_test

import (
	"testing"

	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// TestNewServerDetectsFabricDWFromEngineEdition: a Server from NewServer has no
// hostname, so EngineEdition 11 alone makes it Fabric, and nothing else does.
func TestNewServerDetectsFabricDWFromEngineEdition(t *testing.T) {
	for _, tc := range []struct {
		edition gosmo.EngineEdition
		want    bool
	}{
		{gosmo.EngineAzureSynapseSrvls, true},
		{gosmo.EngineEnterprise, false},
		{gosmo.EngineAzureSQLDatabase, false},
		{gosmo.EngineFabricSQLDatabase, false},
	} {
		f := gosmotest.New(t)
		f.Info.EngineEdition = tc.edition
		if got := f.Server().IsFabricDW(); got != tc.want {
			t.Errorf("EngineEdition %d: IsFabricDW = %v, want %v", tc.edition, got, tc.want)
		}
	}
	var nilServer *gosmo.Server
	if nilServer.IsFabricDW() {
		t.Error("nil Server: IsFabricDW = true")
	}
}
