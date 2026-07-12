package gosmo

import "testing"

func TestAuthMethodIsEntraMethod(t *testing.T) {
	nonEntra := []AuthMethod{AuthSQLServer, AuthWindows}
	for _, m := range nonEntra {
		if m.isEntraMethod() {
			t.Errorf("AuthMethod(%d).isEntraMethod() = true, want false", m)
		}
	}

	entra := []AuthMethod{
		AuthEntraDefault, AuthEntraPassword, AuthEntraMSI,
		AuthEntraServicePrincipal, AuthEntraServicePrincipalAccessToken,
		AuthEntraIntegrated, AuthEntraInteractive, AuthEntraDeviceCode,
		AuthEntraAzCLI, AuthEntraAzureDeveloperCLI, AuthEntraAzurePipelines,
		AuthEntraOnBehalfOf,
	}
	for _, m := range entra {
		if !m.isEntraMethod() {
			t.Errorf("AuthMethod(%d).isEntraMethod() = false, want true", m)
		}
	}
}

// TestFedauthValueCoversAllEntraMethods guards against a new AuthEntra*
// constant being added to auth.go without a matching fedauthValue entry —
// buildDSN would otherwise silently fail with "unsupported auth method"
// for every caller of the new method.
func TestFedauthValueCoversAllEntraMethods(t *testing.T) {
	entra := []AuthMethod{
		AuthEntraDefault, AuthEntraPassword, AuthEntraMSI,
		AuthEntraServicePrincipal, AuthEntraServicePrincipalAccessToken,
		AuthEntraIntegrated, AuthEntraInteractive, AuthEntraDeviceCode,
		AuthEntraAzCLI, AuthEntraAzureDeveloperCLI, AuthEntraAzurePipelines,
		AuthEntraOnBehalfOf,
	}
	for _, m := range entra {
		if _, ok := fedauthValue[m]; !ok {
			t.Errorf("fedauthValue is missing an entry for AuthMethod(%d)", m)
		}
	}
	if len(fedauthValue) != len(entra) {
		t.Errorf("fedauthValue has %d entries, want %d (one per Entra AuthMethod)", len(fedauthValue), len(entra))
	}
}
