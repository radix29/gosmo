package gosmo

import "testing"

// TestDetectFabricDW pins Server.IsFabricDW's rule: EngineEdition 11 is Fabric
// unless the host is a Synapse serverless endpoint, any other known edition is
// not, and an unknown one falls back to the Fabric hostname.
func TestDetectFabricDW(t *testing.T) {
	const fabricHost = "warehouse.datawarehouse.fabric.microsoft.com"
	for _, tc := range []struct {
		edition EngineEdition
		host    string
		want    bool
	}{
		{EngineAzureSynapseSrvls, fabricHost, true},
		{EngineAzureSynapseSrvls, "WAREHOUSE.MSIT-DATAWAREHOUSE.FABRIC.MICROSOFT.COM.", true},
		{EngineAzureSynapseSrvls, "warehouse.contoso.com", true}, // a DNS alias
		{EngineAzureSynapseSrvls, "10.0.0.4", true},              // a private endpoint by address
		{EngineAzureSynapseSrvls, "", true},                      // NewServer: no host
		{EngineAzureSynapseSrvls, "ws-ondemand.sql.azuresynapse.net", false},
		{EngineAzureSynapseSrvls, "WS-ONDEMAND.SQL.AZURESYNAPSE.NET.", false},
		{EngineAzureSynapseSrvls, "ws-ondemand.privatelink.sql.azuresynapse.net", false},
		{EngineAzureSynapseSrvls, "ws-ondemand.sql.azuresynapse.azure.cn", false},
		{EngineAzureSynapseSrvls, "ws-ondemand.sql.azuresynapse.usgovcloudapi.net", false},
		{EngineAzureSynapseSrvls, "sql.azuresynapse.net.example.org", true},
		{EngineEnterprise, fabricHost, false},
		{EngineStandard, "", false},
		{EngineAzureSQLDatabase, fabricHost, false},
		{EngineFabricSQLDatabase, fabricHost, false},
		{EngineAzureSynapse, fabricHost, false},
		{0, fabricHost, true},
		{0, "warehouse.other-datawarehouse.fabric.microsoft.com", true},
		{0, fabricHost + ".", true},
		{0, fabricHost + ".example.org", false},
		{0, "localhost", false},
		{0, "", false},
		{42, fabricHost, true}, // an edition added after this was written
		{42, "localhost", false},
	} {
		if got := detectFabricDW(tc.edition, tc.host); got != tc.want {
			t.Errorf("detectFabricDW(%d, %q) = %v, want %v", tc.edition, tc.host, got, tc.want)
		}
	}
}

// TestConnectionOptionsFabricDW: Connect's decision reads the host out of
// every Server form, and FabricDW overrides detection either way.
func TestConnectionOptionsFabricDW(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		server   string
		edition  EngineEdition
		override *bool
		want     bool
	}{
		{"warehouse.datawarehouse.fabric.microsoft.com", EngineAzureSynapseSrvls, nil, true},
		{"tcp:warehouse.datawarehouse.fabric.microsoft.com,1433", 0, nil, true},
		{" warehouse.datawarehouse.fabric.microsoft.com:1433 ", 0, nil, true},
		{"tcp:ws-ondemand.sql.azuresynapse.net,1433", EngineAzureSynapseSrvls, nil, false},
		{`win10cli\SQL2017`, EngineEnterprise, nil, false},
		{`win10cli\SQL2017`, EngineEnterprise, &yes, true},
		{"warehouse.datawarehouse.fabric.microsoft.com", EngineAzureSynapseSrvls, &no, false},
		{"ws-ondemand.sql.azuresynapse.net", EngineAzureSynapseSrvls, &yes, true},
	} {
		o := ConnectionOptions{Server: tc.server, FabricDW: tc.override}
		if got := o.fabricDW(tc.edition); got != tc.want {
			t.Errorf("Server %q, edition %d, override %v: fabricDW = %v, want %v",
				tc.server, tc.edition, tc.override, got, tc.want)
		}
	}
}
