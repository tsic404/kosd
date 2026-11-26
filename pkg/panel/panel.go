// Package panel assembles the control plane's panel: the pages the active
// components declare and the readers that resolve their server-source items
// (KOSD-C-22, KOSD-C-53). The production process and the acceptance harness both
// build the panel here, so the two cannot wire different readers.
package panel

import (
	"time"

	"k8s.io/client-go/dynamic"

	devicemanager "github.com/tsic404/kos-devicemanager/pkg/devicemanager"
	kosdiagnostics "github.com/tsic404/kos-diagnostics"
	taskmanager "github.com/tsic404/kos-taskmanager/app"

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
	// Audit reads the control-plane audit records the maintenance origin comes
	// from (KOSD-C-14, KOSD-C-82); nil leaves an unschedulable node's origin
	// unknown.
	Audit taskmanager.AuditReader
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
// The first-party product readers are registered here rather than by a caller:
// the readers ship with the control plane, so they stay in the table while the
// pages themselves appear and disappear with the declarations that declare them
// (KOSD-C-28, KOSD-C-29). The page declarations stay with the products; only
// the read side is hosted here (KOSD-C-53). A caller-supplied reader for the
// same identifier wins, so a host that serves its own build of the product is
// not silently replaced.
func New(declarations pages.Declarations, config Config) *pages.Service {
	router := make(pages.Router, len(config.Products)+3)
	for _, product := range config.Products {
		router[product.Identifier] = product.Readings
	}
	if _, supplied := router[kosdiagnostics.Identifier]; !supplied {
		router[kosdiagnostics.Identifier] = kosdiagnostics.NewReader(config.Dynamic, config.Environment, config.Now)
	}
	if _, supplied := router[devicemanager.Identifier]; !supplied {
		router[devicemanager.Identifier] = &devicemanager.Readings{Dynamic: config.Dynamic, Now: config.Now}
	}
	if _, supplied := router[taskmanager.Identifier]; !supplied {
		router[taskmanager.Identifier] = &taskmanager.Readings{Dynamic: config.Dynamic, Audit: config.Audit, Now: config.Now}
	}
	return pages.New(declarations, pages.Config{
		Readings: router,
		Now:      config.Now,
		Banners:  config.Banners,
	})
}
