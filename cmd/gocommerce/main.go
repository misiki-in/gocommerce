// Command gocommerce is the reference binary: a store with no modules
// installed. It is useful for migrating a database, inspecting the API
// contract, and proving the engine boots.
//
// A real store is its own tiny main() that composes the engine with the
// modules it needs — see examples/store.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/misiki/gocommerce/core"
	"github.com/misiki/gocommerce/ext/identity"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gocommerce:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gocommerce", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `GoCommerce — a small, composable commerce engine for Go

usage:
  gocommerce [flags] <command>

commands:
  serve      apply migrations, then serve the API (default)
  migrate    apply migrations and exit
  superuser  create or update an admin-panel operator, then exit
  doctor     run operational diagnostics and exit (-json for agents)
  spec       print the OpenAPI contract and exit
  taxonomy   import a product category tree, then exit
  attributes import the fields categories ask of a product, then exit
  version    print the engine version and exit

  superuser usage:
    gocommerce superuser create <email> <password>
    gocommerce superuser update <email> <password>
    gocommerce superuser list

  taxonomy usage:
    gocommerce taxonomy import            Shopify's standard taxonomy (embedded),
                                          and the fields that go with it
    gocommerce taxonomy import <file>     the same format, from a file or -

  attributes usage:
    gocommerce attributes import          Shopify's field definitions (embedded),
                                          for a tree already imported
    gocommerce attributes import <file>   the same format, from a file or -

flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
environment:
  DATABASE_URL      PostgreSQL connection string, used when -db is not given
  GOCOMMERCE_ADMIN_TOKEN
                    admin bearer token, used when -admin-token is not given
                    (several may be given, comma-separated)
  GOCOMMERCE_ADMIN_EMAIL, GOCOMMERCE_ADMIN_PASSWORD
                    if set, "serve" creates this superuser when the database
                    has none, so an unattended deploy comes up signed-in-able
  GOCOMMERCE_MEDIA_DIR
                    where uploaded media is written, used when -media-dir is
                    not given; leaving it unset disables uploads and the media
                    library records files by URL only
  GOCOMMERCE_IDENTITY_RESET_URL
                    with -identity, the storefront page a password-reset email
                    links to, with {token} where the token goes
`)
	}

	var (
		dbURL        = fs.String("db", "", "PostgreSQL URL (default $DATABASE_URL)")
		addr         = fs.String("addr", ":8080", "listen address")
		tokens       = fs.String("admin-token", "", "admin bearer token(s), comma-separated (default $GOCOMMERCE_ADMIN_TOKEN)")
		currency     = fs.String("currency", gocommerce.DefaultCurrency, "store settlement currency (ISO 4217)")
		langs        = fs.String("languages", gocommerce.DefaultLanguage, "served languages, comma-separated; the first is the default")
		dev          = fs.Bool("dev", false, "development mode: permits booting with no admin token")
		verbose      = fs.Bool("v", false, "verbose (debug) logging")
		jsonOut      = fs.Bool("json", false, "machine-readable output (doctor)")
		mediaDir     = fs.String("media-dir", "", "directory for uploaded media (default $GOCOMMERCE_MEDIA_DIR; empty disables uploads)")
		withIdentity = fs.Bool("identity", false, "install the identity module: shopper accounts under /x/identity/ (guest checkout stays)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	command := "serve"
	if fs.NArg() > 0 {
		command = fs.Arg(0)
	}
	if command == "version" {
		fmt.Println("GoCommerce", gocommerce.Version)
		return nil
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *dbURL == "" {
		*dbURL = os.Getenv("DATABASE_URL")
	}
	if *dbURL == "" {
		return errors.New("no database URL: pass -db or set DATABASE_URL")
	}
	if *tokens == "" {
		*tokens = os.Getenv("GOCOMMERCE_ADMIN_TOKEN")
	}

	// Only `serve` exposes an HTTP surface, so only `serve` needs an admin
	// token. Requiring one for migrate/superuser/doctor/spec would mean the
	// diagnostic command refuses to run on precisely the store it exists to
	// diagnose: one whose operators are superusers and which has no static
	// token at all.
	offline := command != "serve"

	languages := splitList(*langs)
	cfg := gocommerce.Config{
		DBURL:       *dbURL,
		Addr:        *addr,
		Currency:    *currency,
		Languages:   languages,
		AdminTokens: splitList(*tokens),
		Dev:         *dev || offline,
		Logger:      log,
	}
	if *mediaDir == "" {
		*mediaDir = os.Getenv("GOCOMMERCE_MEDIA_DIR")
	}
	cfg.MediaDir = *mediaDir
	if len(languages) > 0 {
		cfg.DefaultLanguage = languages[0]
	}

	// The reference binary installs no module by default — that is what
	// proves the engine boots on its own. Accounts are the one capability a
	// storefront asks for often enough that a flag beats a fork of main().
	var modules []gocommerce.Module
	if *withIdentity {
		modules = append(modules, identity.New(identity.Config{
			ResetURL: os.Getenv("GOCOMMERCE_IDENTITY_RESET_URL"),
		}))
	}

	app, err := gocommerce.New(cfg, modules...)
	if err != nil {
		return err
	}

	switch command {
	case "migrate":
		defer app.Close()
		// New already applied every pending migration; calling it again is
		// harmless and makes the command's intent explicit in the logs.
		if err := app.Migrate(context.Background()); err != nil {
			return err
		}
		log.Info("migrations up to date")
		return nil

	case "superuser":
		defer app.Close()
		return superuserCmd(context.Background(), app, fs.Args()[1:])

	case "doctor":
		defer app.Close()
		return doctorCmd(context.Background(), app, *jsonOut)

	case "spec":
		defer app.Close()
		_, err := os.Stdout.Write(app.Spec())
		return err

	case "taxonomy":
		defer app.Close()
		return taxonomyCmd(context.Background(), app, fs.Args()[1:], log)

	case "attributes":
		defer app.Close()
		return attributesCmd(context.Background(), app, fs.Args()[1:], log)

	case "serve":
		if err := bootstrapSuperuser(context.Background(), app, log); err != nil {
			app.Close()
			return err
		}
		return app.ListenAndServe()

	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

// bootstrapSuperuser creates the first operator from the environment, so a
// container can come up with a usable panel and no manual step. It is
// deliberately create-only: if a superuser already exists, a stale environment
// variable must not silently reset that operator's password.
func bootstrapSuperuser(ctx context.Context, app *gocommerce.App, log *slog.Logger) error {
	email := os.Getenv("GOCOMMERCE_ADMIN_EMAIL")
	password := os.Getenv("GOCOMMERCE_ADMIN_PASSWORD")
	if email == "" || password == "" {
		return nil
	}
	su, created, err := app.Superusers().Bootstrap(ctx, email, password)
	if err != nil {
		return fmt.Errorf("bootstrap superuser: %w", err)
	}
	if created {
		log.Info("created the first superuser from the environment", "email", su.Email)
	}
	return nil
}

// doctorCmd renders the health report for whoever is asking.
//
// It exits non-zero when a check fails, so CI and agents can gate on it
// without parsing anything; -json is for when they want the detail.
func doctorCmd(ctx context.Context, app *gocommerce.App, asJSON bool) error {
	rep := app.Diagnose(ctx)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
		if !rep.OK {
			os.Exit(1)
		}
		return nil
	}

	mark := map[string]string{
		gocommerce.StatusOK:   "ok  ",
		gocommerce.StatusWarn: "warn",
		gocommerce.StatusFail: "FAIL",
	}
	fmt.Printf("GoCommerce %s — %s\n\n", rep.Version, rep.At.Format(time.RFC3339))
	for _, c := range rep.Checks {
		fmt.Printf("  %s  %-18s %s\n", mark[c.Status], c.Name, c.Detail)
		if c.Hint != "" {
			fmt.Printf("        %-18s → %s\n", "", c.Hint)
		}
	}

	fmt.Println()
	if rep.OK {
		fmt.Println("healthy")
		return nil
	}
	fmt.Printf("%d check(s) need attention\n", len(rep.Failed()))
	os.Exit(1)
	return nil
}

func superuserCmd(ctx context.Context, app *gocommerce.App, args []string) error {
	sus := app.Superusers()

	if len(args) > 0 && args[0] == "list" {
		list, err := sus.List(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no superusers yet — create one with: gocommerce superuser create <email> <password>")
			return nil
		}
		for _, su := range list {
			fmt.Printf("%d\t%s\t%s\n", su.ID, su.Email, su.CreatedAt.Format("2006-01-02 15:04"))
		}
		return nil
	}

	if len(args) < 3 || (args[0] != "create" && args[0] != "update") {
		return errors.New(
			"usage: gocommerce superuser create|update <email> <password> [role]  |  gocommerce superuser list")
	}
	action, email, password := args[0], args[1], args[2]
	// The role is optional and defaults to owner, so a script that has always
	// created an operator keeps creating the operator it always did.
	role := ""
	if len(args) > 3 {
		role = args[3]
	}

	if action == "create" {
		su, err := sus.Create(ctx, email, password, role)
		if err != nil {
			return err
		}
		fmt.Printf("created superuser %s as %s (id %d)\n", su.Email, su.Role, su.ID)
		return nil
	}

	list, err := sus.List(ctx)
	if err != nil {
		return err
	}
	for _, su := range list {
		if strings.EqualFold(su.Email, strings.TrimSpace(email)) {
			if _, err := sus.Update(ctx, su.ID, "", password); err != nil {
				return err
			}
			fmt.Printf("updated the password for %s; their other sessions were signed out\n", su.Email)
			return nil
		}
	}
	return fmt.Errorf("no superuser with email %q", email)
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// taxonomyCmd imports a category tree.
//
// It is a command rather than a migration because the tree is the operator's
// data: a store that wants six categories of its own should not find fourteen
// thousand of Shopify's in it because it upgraded. Running it twice is safe —
// see Categories.ImportTaxonomy — so it is also the way to top up an existing
// tree after the published taxonomy grows.
func taxonomyCmd(ctx context.Context, app *gocommerce.App, args []string, log *slog.Logger) error {
	if len(args) == 0 || args[0] != "import" {
		return errors.New("usage: gocommerce taxonomy import [file|-]")
	}

	var reader io.Reader = strings.NewReader(gocommerce.ShopifyTaxonomy())
	source := "the embedded Shopify taxonomy"
	if len(args) > 1 {
		switch args[1] {
		case "-":
			reader, source = os.Stdin, "stdin"
		default:
			f, err := os.Open(args[1])
			if err != nil {
				return fmt.Errorf("open %s: %w", args[1], err)
			}
			defer f.Close()
			reader, source = f, args[1]
		}
	}

	log.Info("importing categories", "source", source)
	result, err := app.Categories().ImportTaxonomy(ctx, reader)
	if err != nil {
		return err
	}
	log.Info("categories imported",
		"created", result.Created, "already present", result.Matched, "skipped", result.Skipped)

	// The fields each category asks of a product follow the tree, and only for
	// the embedded source: they are matched by the taxonomy id ImportTaxonomy
	// wrote, so a tree that came from somewhere else has nothing to match. An
	// operator who wants both from their own files runs the two commands.
	if len(args) > 1 {
		return nil
	}
	attrs, err := app.Categories().ImportCategoryAttributes(
		ctx, strings.NewReader(gocommerce.ShopifyCategoryAttributes()))
	if err != nil {
		return err
	}
	log.Info("category fields imported",
		"attributes", attrs.Attributes, "categories", attrs.Categories,
		"unmatched", attrs.Unmatched, "skipped", attrs.Skipped)
	return nil
}

// attributesCmd imports only the field definitions, for a tree that is already
// in place.
func attributesCmd(ctx context.Context, app *gocommerce.App, args []string, log *slog.Logger) error {
	if len(args) == 0 || args[0] != "import" {
		return errors.New("usage: gocommerce attributes import [file|-]")
	}

	var reader io.Reader = strings.NewReader(gocommerce.ShopifyCategoryAttributes())
	source := "the embedded Shopify attributes"
	if len(args) > 1 {
		switch args[1] {
		case "-":
			reader, source = os.Stdin, "stdin"
		default:
			f, err := os.Open(args[1])
			if err != nil {
				return fmt.Errorf("open %s: %w", args[1], err)
			}
			defer f.Close()
			reader, source = f, args[1]
		}
	}

	log.Info("importing category fields", "source", source)
	result, err := app.Categories().ImportCategoryAttributes(ctx, reader)
	if err != nil {
		return err
	}
	log.Info("category fields imported",
		"attributes", result.Attributes, "categories", result.Categories,
		"unmatched", result.Unmatched, "skipped", result.Skipped)
	return nil
}
