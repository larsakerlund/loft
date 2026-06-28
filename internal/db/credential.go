// The Postgres credential seam. A connection string carries its own password (local dev, or any
// deployment with a static secret), so it needs no provider. The host-based path instead asks a
// CredentialProvider for a password on every new physical connection, which is how rotating cloud
// credentials (a managed-identity token) stay fresh without a stored secret.
package db

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/larsakerlund/loft/internal/config"
)

const pgScope = "https://ossrdbms-aad.database.windows.net/.default"

// CredentialProvider supplies the Postgres password for a new connection. Password is called per
// physical connection so a short-lived token is refreshed rather than pinned for the pool's life.
type CredentialProvider interface {
	Password(ctx context.Context) (string, error)
}

// newCredentialProvider selects the credential source for host-based auth. Today that is an Azure
// managed identity (no stored secret). A static-password deployment uses LOFT_PG_CONNECTION_STRING
// instead and never reaches here; another cloud's IAM auth would be a new branch returning its own
// provider.
func newCredentialProvider(cfg config.Config) (CredentialProvider, error) {
	var opts *azidentity.ManagedIdentityCredentialOptions
	if cfg.UAMIClientID != "" {
		opts = &azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(cfg.UAMIClientID)}
	}
	cred, err := azidentity.NewManagedIdentityCredential(opts)
	if err != nil {
		return nil, fmt.Errorf("managed identity: %w", err)
	}
	return &azureMICredential{cred: cred}, nil
}

// azureMICredential uses an Entra access token as the Postgres password.
type azureMICredential struct {
	cred azcore.TokenCredential
}

// Password returns a fresh Entra access token to use as the Postgres password. It is called per new
// connection, so a rotating managed-identity credential stays current.
func (a *azureMICredential) Password(ctx context.Context) (string, error) {
	tok, err := a.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{pgScope}})
	if err != nil {
		return "", fmt.Errorf("postgres token: %w", err)
	}
	return tok.Token, nil
}
