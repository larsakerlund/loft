// Package cli implements the `loft` deploy CLI. It bundles a folder and uploads it to loftd over
// HTTP, authenticated with a token from `loft login` (the OAuth device flow). The CLI talks only to
// loftd's public API, never to any cloud storage directly, so it is the same regardless of where a
// deployment is hosted. Guardrails run entirely locally before any auth or network.
package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrUsage signals a usage error (unknown command). The caller should exit with code 2.
var ErrUsage = errors.New("usage")

const brand = "loft"

// Run dispatches a CLI invocation. It returns ErrUsage for an unknown command, nil for help/success.
func Run(ctx context.Context, args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "deploy":
		return runDeploy(ctx, args[1:])
	case "delete":
		return runDelete(ctx, args[1:])
	case "login":
		return runLogin(ctx, args[1:])
	case "whoami":
		return runWhoami(ctx, args[1:])
	case "", "help", "-h", "--help":
		usage()
		return nil
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		return ErrUsage
	}
}

// resolveBase finds the platform URL: --url flag, then LOFT_URL, then the saved login.
func resolveBase(flags map[string]string) string {
	if u := firstNonEmpty(flags["url"], os.Getenv("LOFT_URL")); u != "" {
		return u
	}
	if c, err := loadCredentials(); err == nil {
		return c.URL
	}
	return ""
}

func runDeploy(ctx context.Context, args []string) error {
	pos, flags := parseArgs(args)
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	_, force := flags["force"]
	name := sanitizeSite(firstNonEmpty(flags["name"], at(pos, 1), filepath.Base(dir)))

	// Validate locally before any network/auth, so bad input fails instantly with every reason.
	entries, err := collect(dir)
	if err != nil {
		return err
	}
	if err := validate(entries); err != nil {
		return err
	}

	base := resolveBase(flags)
	if base == "" {
		return errors.New("no platform URL: pass --url, set LOFT_URL, or run `loft login`")
	}
	client := newClient(base, resolveToken(base))

	res, err := deployWithConfirm(ctx, client, name, entries, force)
	if err != nil {
		return err
	}
	fmt.Printf("✓ deployed %d files to %s\n", res.Files, siteURL(base, res.Site))
	return nil
}

// deployWithConfirm uploads, and if the site already exists (and --force was not given), asks the
// user to confirm by retyping the name before retrying with overwrite. Overwriting replaces the site
// and prunes files no longer present, so it is a deliberate act, the same as delete.
func deployWithConfirm(ctx context.Context, client *loftClient, name string, entries []fileEntry, force bool) (deployResult, error) {
	sp := startSpinner(fmt.Sprintf("deploying %d files", len(entries)))
	res, err := client.deploy(ctx, name, entries, force)
	sp.stop()
	if !errors.Is(err, errSiteExists) {
		return res, err
	}
	fmt.Printf("\nSite %q already exists. Overwriting replaces it and removes files no longer present.\n", name)
	if ask(fmt.Sprintf("? Type '%s' to confirm overwrite: ", name)) != name {
		return deployResult{}, errors.New("aborted (name did not match)")
	}
	sp = startSpinner("overwriting")
	res, err = client.deploy(ctx, name, entries, true)
	sp.stop()
	return res, err
}

func runDelete(ctx context.Context, args []string) error {
	pos, flags := parseArgs(args)
	name := sanitizeSite(firstNonEmpty(flags["name"], at(pos, 0)))
	if name == "" {
		return errors.New("usage: loft delete <name>")
	}
	base := resolveBase(flags)
	if base == "" {
		return errors.New("no platform URL: pass --url, set LOFT_URL, or run `loft login`")
	}
	if _, force := flags["force"]; !force {
		if ask(fmt.Sprintf("? Type '%s' to confirm deletion: ", name)) != name {
			return errors.New("aborted (name did not match)")
		}
	}
	client := newClient(base, resolveToken(base))
	sp := startSpinner("deleting site")
	err := client.delete(ctx, name)
	sp.stop()
	if err != nil {
		return err
	}
	fmt.Printf("✓ deleted site %q\n", name)
	return nil
}

func runLogin(ctx context.Context, args []string) error {
	pos, flags := parseArgs(args)
	existing, _ := loadCredentials()
	base := firstNonEmpty(flags["url"], at(pos, 0), os.Getenv("LOFT_URL"), existing.URL)

	// Discover the OAuth config from the platform URL, so `loft login <url>` is the whole setup. Any
	// of these can still be pinned by flag/env, which also covers a provider with no discovery.
	var found cliConfig
	discovered := false
	if base != "" {
		if c, err := discoverConfig(ctx, base); err == nil {
			found, discovered = c, true
		} else {
			fmt.Fprintf(os.Stderr, "note: %v\n", err)
		}
	}
	issuer := firstNonEmpty(flags["issuer"], os.Getenv("LOFT_OIDC_ISSUER"), found.Issuer, existing.Issuer)
	clientID := firstNonEmpty(flags["client-id"], os.Getenv("LOFT_CLI_CLIENT_ID"), found.ClientID, existing.ClientID)
	scope := firstNonEmpty(flags["scope"], os.Getenv("LOFT_CLI_SCOPE"), found.Scope, existing.Scope, "openid offline_access")

	// An auth-off platform (e.g. local dev) advertises no issuer: there's no token to get, but we
	// still save the URL so `loft deploy` needs no --url.
	if issuer == "" && discovered {
		if err := saveCredentials(credentials{URL: strings.TrimRight(base, "/")}); err != nil {
			return err
		}
		fmt.Printf("✓ using %s — no sign-in required (auth is off)\n", base)
		return nil
	}
	if issuer == "" || clientID == "" {
		return errors.New("could not configure login: pass a platform URL (loft login <url>) that advertises CLI config, or set --issuer/--client-id")
	}

	token, err := deviceLogin(ctx, issuer, clientID, scope)
	if err != nil {
		return err
	}
	if err := saveCredentials(credentials{
		URL:         strings.TrimRight(base, "/"),
		Issuer:      issuer,
		ClientID:    clientID,
		Scope:       scope,
		AccessToken: token,
	}); err != nil {
		return err
	}
	fmt.Println("✓ logged in")
	if base != "" {
		if me, err := newClient(base, token).me(ctx); err == nil {
			fmt.Printf("  signed in as %s\n", me)
		}
	}
	return nil
}

func runWhoami(ctx context.Context, args []string) error {
	_, flags := parseArgs(args)
	base := resolveBase(flags)
	if base == "" {
		return errors.New("no platform URL: pass --url, set LOFT_URL, or run `loft login`")
	}
	me, err := newClient(base, resolveToken(base)).me(ctx)
	if err != nil {
		return err
	}
	fmt.Println(me)
	return nil
}

// siteURL builds the URL a deployed site serves at: the site label as a subdomain of the platform
// host (e.g. base https://loft.example.com, site "blog" → https://blog.loft.example.com).
func siteURL(base, site string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return site
	}
	return fmt.Sprintf("%s://%s.%s", u.Scheme, site, u.Host)
}
