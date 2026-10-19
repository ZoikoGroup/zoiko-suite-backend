package providers

import "zoiko.io/eventing/adapter/ext"

// The connector resolver and the vault credential source are shared with the other provider adapters; they
// live in eventing/adapter/ext and are aliased here so this package's API is unchanged.
type (
	Connector         = ext.Connector
	Caller            = ext.Caller
	ConnectorResolver = ext.ConnectorResolver
	CredentialSource  = ext.CredentialSource
	ConnectorHTTP     = ext.ConnectorHTTP
	VaultHTTP         = ext.VaultHTTP
)

var (
	NewConnectorHTTP = ext.NewConnectorHTTP
	NewVaultHTTP     = ext.NewVaultHTTP
)
