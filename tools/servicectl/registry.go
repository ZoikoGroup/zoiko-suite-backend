// Service registry: what can be run, on which port, with which environment.
//
// The table itself is generated (registry_gen.go) from the compose files and the
// console's service registry. Everything in this file is the part that is read
// by hand: lookups, page resolution, and the invariants that must hold over the
// generated data.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Service is one runnable Go service.
type Service struct {
	// Name is the directory under services/ AND the service's identity
	// everywhere else -- log lines, the HTTP API, the dashboard. There is no
	// second short alias, deliberately: the console has its own aliases
	// (`contracts` for contract-lifecycle-svc) and carrying them here would
	// make one service answer to two names in one process.
	Name string

	// Port is allocated, not conventional. See registry_gen.go's header for why
	// the compose files could not supply these directly.
	Port int

	// DBName is the logical database this service owns -- `general_ledger`.
	// Under a local Postgres it is a DATABASE; on a single-database managed host
	// it is a SCHEMA. env.go decides which, from ZOIKO_DB_PROVIDER. Empty means
	// the service has no database and gets no DB_* variables at all.
	DBName string

	// DBRole is the login role this service connects as on a managed host --
	// `app_general_ledger`. Filled in by LoadRegistry from DBName, so it is not
	// in the generated table.
	//
	// ONE ROLE PER SERVICE IS THE WHOLE POINT. The isolation that used to come
	// from 63 separate databases now comes from each role holding USAGE on its
	// own schema and no other. A single shared role would connect every service
	// with access to every schema and quietly undo that.
	DBRole string

	// Env is this service's own environment: cross-service URLs, its Kafka
	// topic, its feature flags. Nothing global lives here -- credentials, hosts
	// and shared secrets come from the global env file, so there is exactly one
	// place to change a password.
	Env map[string]string
}

// Registry is the loaded, validated service table.
type Registry struct {
	all    []*Service
	byName map[string]*Service
	pages  map[string][]string
}

// LoadRegistry validates the generated table and returns it.
//
// The validation is not ceremony. The generator derives ports from four compose
// files that between them claimed sixteen ports twice, so a duplicate here is
// the single most likely way this table goes wrong -- and it would present as
// one service intermittently failing to bind, which is a miserable thing to
// debug from the far end.
func LoadRegistry() (*Registry, error) {
	r := &Registry{
		byName: make(map[string]*Service, len(generatedServices)),
		pages:  generatedPages,
	}
	seenPort := make(map[int]string, len(generatedServices))

	for i := range generatedServices {
		svc := &generatedServices[i]
		if svc.Name == "" {
			return nil, fmt.Errorf("registry entry %d has no name", i)
		}
		if prev, dup := r.byName[svc.Name]; dup {
			return nil, fmt.Errorf("service %q listed twice (ports %d and %d)", svc.Name, prev.Port, svc.Port)
		}
		if svc.Port < 1024 || svc.Port > 65535 {
			return nil, fmt.Errorf("service %q has port %d, outside the usable range", svc.Name, svc.Port)
		}
		if prev, dup := seenPort[svc.Port]; dup {
			return nil, fmt.Errorf("port %d claimed by both %q and %q", svc.Port, prev, svc.Name)
		}
		seenPort[svc.Port] = svc.Name
		svc.DBRole = dbRoleFor(svc.DBName)
		r.byName[svc.Name] = svc
		r.all = append(r.all, svc)
	}

	// A page naming a service that does not exist would silently start nothing,
	// so it is caught here rather than at request time.
	for page, names := range r.pages {
		for _, n := range names {
			if _, ok := r.byName[n]; !ok {
				return nil, fmt.Errorf("page %q names unknown service %q", page, n)
			}
		}
	}

	sort.Slice(r.all, func(i, j int) bool { return r.all[i].Name < r.all[j].Name })
	return r, nil
}

// dbRoleExceptions holds the schemas whose login role does not follow the
// app_<schema> convention.
//
// There is exactly one. authorization-svc's schema is `authorization_svc` but
// its role is `app_authorization` -- deployments/supabase/main.go and its README
// both say so. Deriving `app_authorization_svc` instead fails as an
// authentication error, which through a Supabase pooler is indistinguishable
// from a wrong password and sends you looking at APP_DB_PASSWORD.
var dbRoleExceptions = map[string]string{
	"authorization_svc": "app_authorization",
}

// dbRoleFor returns the login role for a schema. The convention is app_<schema>,
// which is what deployments/scripts/create-app-roles.sh uses for all 21 of its
// entries without exception.
func dbRoleFor(schema string) string {
	if schema == "" {
		return ""
	}
	if role, ok := dbRoleExceptions[schema]; ok {
		return role
	}
	return "app_" + schema
}

// Get returns a service by name.
func (r *Registry) Get(name string) (*Service, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// All returns every service, name-ordered.
func (r *Registry) All() []*Service { return r.all }

// Pages returns the route-to-services map.
func (r *Registry) Pages() map[string][]string { return r.pages }

// ServicesForPage resolves a console path to the services that path reads.
//
// Matching is longest-prefix on whole segments, so /admin/legal/<contractId>
// resolves to /admin/legal's services. Segment-wise rather than string-wise
// because a raw strings.HasPrefix would match /admin/legalese against
// /admin/legal, and this map is the difference between a page's panels having
// data and being empty.
func (r *Registry) ServicesForPage(path string) (names []string, matched string) {
	clean := "/" + strings.Trim(path, "/")
	for page, svcs := range r.pages {
		if clean == page || strings.HasPrefix(clean, page+"/") {
			if len(page) > len(matched) {
				matched, names = page, svcs
			}
		}
	}
	return names, matched
}

// Resolve expands a mixed list of service names and console paths into a
// deduplicated service list. Anything starting with "/" is treated as a path.
func (r *Registry) Resolve(items []string) (names []string, unknown []string) {
	seen := make(map[string]bool)
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if strings.HasPrefix(it, "/") {
			svcs, matched := r.ServicesForPage(it)
			if matched == "" {
				unknown = append(unknown, it)
				continue
			}
			for _, s := range svcs {
				add(s)
			}
			continue
		}
		if _, ok := r.byName[it]; !ok {
			unknown = append(unknown, it)
			continue
		}
		add(it)
	}
	sort.Strings(names)
	return names, unknown
}
