// Package panel assembles the control plane's panel: the pages the active
// components declare and the readers that resolve their server-source items
// (KOSD-C-22, KOSD-C-53). The production process and the acceptance harness both
// build the panel here, so the two cannot wire different readers.
package panel

import (
	"time"

	"k8s.io/client-go/dynamic"

	kosdiagnostics "github.com/tsic404/kos-diagnostics"

	"github.com/tsic404/kosd/pkg/diagnostics"
	"github.com/tsic404/kosd/pkg/pages"
)

// Product is one extension product whose page items the host resolves itself.
// Identifier is the prefix the product's item references start with.
type Product struct {
	Identifier string
	Readings   pages.Readings
}

// Config carries what the panel needs from the process that serves it.
type Config struct {
	// Dynamic reads the cluster objects the product readers project (KOSD-C-12).
	Dynamic dynamic.Interface
	// Environment is the extension state the diagnostics reader reports
	// (KOSD-C-26); an empty value leaves extension segments unobservable.
	Environment diagnostics.Environment
	// Now supplies the read clock; nil selects time.Now.
	Now func() time.Time
	// Banners supplies the shell-level notice; nil shows none (KOSD-C-56).
	Banners pages.Banners
	// Products are further product readers, keyed by product identifier. They
	// join the same table the products this process hosts are registered in.
	Products []Product
}

// New builds the page shell with every product reader registered in one table.
//
// The diagnostics reader is registered here rather than by a caller: the
// product ships with the control plane, so the reader stays in the table while
// the page itself appears and disappears with the declaration that declares it
// (KOSD-C-28, KOSD-C-29). A caller-supplied reader for the same identifier
// wins, so a host that serves its own build of the product is not silently
// replaced.
func New(declarations pages.Declarations, config Config) *pages.Service {
	router := make(pages.Router, len(config.Products)+1)
	for _, product := range config.Products {
		router[product.Identifier] = product.Readings
	}
	if _, supplied := router[kosdiagnostics.Identifier]; !supplied {
		router[kosdiagnostics.Identifier] = kosdiagnostics.NewReader(config.Dynamic, config.Environment, config.Now)
	}
	return pages.New(declarations, pages.Config{
		Readings: router,
		Now:      config.Now,
		Banners:  config.Banners,
	})
}
