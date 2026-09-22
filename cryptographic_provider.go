package gosmo

import (
	"fmt"
	"strings"
)

// cryptographic_provider.go covers the FROM PROVIDER clause an asymmetric or
// symmetric key held by an Extensible Key Management provider is created
// with. The providers themselves are listed by Server.CryptographicProviders
// (credential.go).
//
// Nothing here has been run against a real provider: EKM needs a vendor DLL
// registered with CREATE CRYPTOGRAPHIC PROVIDER and 'EKM provider enabled'
// switched on, and no test instance has one. The statements follow the
// documented grammar and are pinned by unit tests only.

// ProviderKeyDisposition is CREATION_DISPOSITION: whether the provider makes
// a new key or maps one it already holds.
type ProviderKeyDisposition string

const (
	ProviderCreateNew    ProviderKeyDisposition = "CREATE_NEW"
	ProviderOpenExisting ProviderKeyDisposition = "OPEN_EXISTING"
)

// ProviderKey is the FROM PROVIDER half of an asymmetric or symmetric key
// spec: which provider holds the key, and under what name it knows it.
type ProviderKey struct {
	// Provider is the cryptographic provider's name; required.
	Provider string

	// KeyName is PROVIDER_KEY_NAME, the key's name inside the provider;
	// required.
	KeyName string

	// Disposition is CREATION_DISPOSITION. Empty omits the clause, which the
	// server takes as CREATE_NEW.
	Disposition ProviderKeyDisposition
}

// clauses renders " FROM PROVIDER [p]" and the WITH options the provider
// half contributes, validating it. The caller adds ALGORITHM, which is the
// key family's own.
func (p *ProviderKey) clauses() (from string, opts []string, err error) {
	if strings.TrimSpace(p.Provider) == "" {
		return "", nil, fmt.Errorf("no cryptographic provider named")
	}
	if strings.TrimSpace(p.KeyName) == "" {
		return "", nil, fmt.Errorf("no provider key name")
	}
	opts = []string{"PROVIDER_KEY_NAME = " + nStringLiteral(p.KeyName)}
	switch p.Disposition {
	case "":
	case ProviderCreateNew, ProviderOpenExisting:
		opts = append(opts, "CREATION_DISPOSITION = "+string(p.Disposition))
	default:
		return "", nil, fmt.Errorf("unknown creation disposition %q", p.Disposition)
	}
	return " FROM PROVIDER " + quoteIdent(p.Provider), opts, nil
}
