// Command store is the canonical shape of a gocommerce store: a small main()
// that composes the engine with the capabilities this particular business
// needs. Everything installed is visible on one screen, and "go to definition"
// works on all of it.
//
// Run it against a local database:
//
//	createdb mystore
//	DATABASE_URL=postgres://localhost/mystore \
//	GOCOMMERCE_ADMIN_TOKEN=$(openssl rand -hex 32) \
//	go run ./examples/store
//
// Every module below is optional. With none of them the store still sells:
// cash on delivery and manual fulfillment are built in, because they need no
// third party.
package main

import (
	"log"
	"log/slog"
	"os"

	"github.com/misiki/gocommerce"

	"github.com/misiki/gocommerce/ext/cms"
	"github.com/misiki/gocommerce/ext/invoices"
	"github.com/misiki/gocommerce/ext/mcp"
	sendgrid "github.com/misiki/gocommerce/ext/notify-sendgrid"
	stripe "github.com/misiki/gocommerce/ext/payments-stripe"
)

func main() {
	log.SetFlags(0)

	modules := []gocommerce.Module{
		// Numbered invoices whenever an order is paid. It subscribes to
		// order.paid and owns its own tables; the engine knows nothing about
		// invoicing.
		invoices.New(invoices.Config{
			SellerName:    "Example Ltd",
			SellerAddress: "1 Commerce Way, London",
			NumberFormat:  "INV-{year}-{seq:05}",
		}),

		// Content pages, served at /x/cms/pages/{slug}.
		cms.New(cms.Config{}),

		// The store as tools for an AI agent, at /api/admin/x/mcp. The admin
		// token is the agent's credential, and every change it makes is
		// recorded in an audit table.
		mcp.New(mcp.Config{ServerName: "example-store"}),
	}

	// Card payments, if the keys are configured. Adding Stripe changes no
	// core code: it registers a payment method, and the engine serves its
	// webhook at /api/checkout/stripe/webhook.
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		modules = append(modules, stripe.New(stripe.Config{
			SecretKey:     key,
			WebhookSecret: mustEnv("STRIPE_WEBHOOK_SECRET"),
		}))
	}

	// Real order emails, if configured. Without it the engine still emits the
	// notifications — they just go to the log instead of a shopper.
	if key := os.Getenv("SENDGRID_API_KEY"); key != "" {
		modules = append(modules, sendgrid.New(sendgrid.Config{
			APIKey:   key,
			From:     "orders@example.com",
			FromName: "Example Ltd",
		}))
	}

	app, err := gocommerce.New(gocommerce.Config{
		DBURL:  mustEnv("DATABASE_URL"),
		Addr:   ":8080",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),

		// One settlement currency per store. Money crosses the API as integer
		// minor units plus this code, so switching to JPY or KWD — which have
		// 0 and 3 decimal places — needs no engine change.
		Currency: "USD",

		// The first language is the default; the engine negotiates the rest
		// per request from ?lang= and Accept-Language.
		Languages: []string{"en"},

		// Several tokens may be configured so one can be rotated out without
		// a window where no token works.
		AdminTokens: []string{mustEnv("GOCOMMERCE_ADMIN_TOKEN")},

		// AdminAuth is the seam where an identity module would replace bearer
		// tokens with sessions, OIDC or RBAC. Guest checkout keeps working
		// either way: shoppers never authenticate.
	}, modules...)
	if err != nil {
		log.Fatal(err)
	}

	// Returns nil after a clean SIGINT/SIGTERM shutdown.
	if err := app.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("environment variable %s is required", key)
	}
	return v
}
