package registry

import (
	"encoding/json"
	"sort"
)

// AuthInfo is what a connection needs to know about an AUTH profile of a contract.
type AuthInfo struct {
	Name            string
	Mechanism       string
	ScopesRequired  []string
	CredentialClass string

	// Inbound verification, with the defaults applied.
	SignatureHeader     string
	SignatureEncoding   string
	SignedContent       string
	TimestampHeader     string
	TimestampFormat     string
	ReplayWindowSeconds int
	APIKeyHeader        string
	SignatureAlgorithm  string
}

// EndpointInfo is what a connection needs to know about an ENDPOINT profile.
type EndpointInfo struct {
	Name    string
	Hosts   []string
	Regions []string
	Schemes []string
}

// CertInfo is the CERTIFICATION profile of a contract.
type CertInfo struct {
	TestPackVersion       string
	RequiredNegativeTests []string
	ExpiryDays            int
}

// Operation returns the registered operation with the given id, or nil: only registered operations are callable.
func (v *ContractVersion) Operation(id string) *Operation {
	for i := range v.Content.Operations {
		if v.Content.Operations[i].ID == id {
			return &v.Content.Operations[i]
		}
	}
	return nil
}

// UsedAuthProfiles returns the AUTH profiles the contract's operations actually reference, by name.
func (v *ContractVersion) UsedAuthProfiles() map[string]AuthInfo {
	used := map[string]bool{}
	for _, o := range v.Content.Operations {
		used[o.AuthProfile] = true
	}
	out := map[string]AuthInfo{}
	for _, p := range v.Content.Profiles {
		if p.Kind != ProfileAuth || !used[p.Name] {
			continue
		}
		var a authSpec
		if json.Unmarshal(p.Spec, &a) != nil {
			continue
		}
		sc := append([]string{}, a.ScopesRequired...)
		sort.Strings(sc)
		out[p.Name] = AuthInfo{Name: p.Name, Mechanism: a.Mechanism, ScopesRequired: sc, CredentialClass: a.CredentialClass,
			SignatureHeader: dflt(a.SignatureHeader, DefaultSignatureHeader), SignatureEncoding: dflt(a.SignatureEncoding, DefaultSignatureEncoding),
			SignedContent: dflt(a.SignedContent, DefaultSignedContent), TimestampHeader: dflt(a.TimestampHeader, DefaultTimestampHeader),
			TimestampFormat: dflt(a.TimestampFormat, DefaultTimestampFormat), ReplayWindowSeconds: dfltInt(a.ReplayWindowSeconds, DefaultReplayWindowSeconds),
			APIKeyHeader: dflt(a.APIKeyHeader, DefaultAPIKeyHeader), SignatureAlgorithm: a.SignatureAlgorithm}
	}
	return out
}

// UsedEndpointProfiles returns the ENDPOINT profiles referenced by outbound operations, by name.
func (v *ContractVersion) UsedEndpointProfiles() map[string]EndpointInfo {
	used := map[string]bool{}
	for _, o := range v.Content.Operations {
		if o.EndpointProfile != "" {
			used[o.EndpointProfile] = true
		}
	}
	out := map[string]EndpointInfo{}
	for _, p := range v.Content.Profiles {
		if p.Kind != ProfileEndpoint || !used[p.Name] {
			continue
		}
		var e endpointSpec
		if json.Unmarshal(p.Spec, &e) != nil {
			continue
		}
		out[p.Name] = EndpointInfo{Name: p.Name, Hosts: e.Hosts, Regions: e.Regions, Schemes: e.Schemes}
	}
	return out
}

// Certification returns the contract's certification profile, or nil.
func (v *ContractVersion) Certification() *CertInfo {
	cs := certificationOf(&v.Content)
	if cs == nil {
		return nil
	}
	return &CertInfo{TestPackVersion: cs.TestPackVersion, RequiredNegativeTests: cs.RequiredNegativeTests, ExpiryDays: cs.ExpiryDays}
}

func dflt(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func dfltInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// MappingFor returns the mapping version of the contract that consumes the given source schema, or nil.
func (v *ContractVersion) MappingFor(sourceSchema string) *MappingVersion {
	for i := range v.MappingVersions {
		if v.MappingVersions[i].SourceSchema == sourceSchema {
			return &v.MappingVersions[i]
		}
	}
	return nil
}

// SchemaByRef returns the schema definition with the given id@version, or nil. There is no fallback: an unknown
// version is unknown.
func (v *ContractVersion) SchemaByRef(ref string) *Schema { return schemaByRef(&v.Content, ref) }

// IdempotencyOf returns the idempotency profile named by an operation, decoded.
func (v *ContractVersion) IdempotencyOf(o *Operation) *IdempotencyInfo {
	p := profileOf(&v.Content, ProfileIdempotency, o.IdempotencyProfile)
	if p == nil {
		return nil
	}
	var s idemSpec
	if json.Unmarshal(p.Spec, &s) != nil {
		return nil
	}
	return &IdempotencyInfo{KeySources: s.KeySources, Scope: s.Scope, RetentionDays: s.RetentionDays, OnBodyChange: s.OnBodyChange}
}

// IdempotencyInfo is an idempotency profile.
type IdempotencyInfo struct {
	KeySources    []string
	Scope         string
	RetentionDays int
	OnBodyChange  string
}
