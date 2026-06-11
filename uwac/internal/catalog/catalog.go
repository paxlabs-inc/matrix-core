// Package catalog assembles the first-party connector registry. It is the one
// place that imports each connector implementation, keeping the connectors
// package free of import cycles (connectors <- google <- catalog).
package catalog

import (
	"github.com/paxlabs-inc/uwac/internal/connectors"
	"github.com/paxlabs-inc/uwac/internal/connectors/google"
)

// Registry returns a registry with every first-party connector registered.
func Registry() (*connectors.Registry, error) {
	r := connectors.NewRegistry()
	for _, c := range []*connectors.Connector{
		google.Connector(),
	} {
		if err := r.Register(c); err != nil {
			return nil, err
		}
	}
	return r, nil
}
