package cli

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func usage() {
	fmt.Printf(`%s — self-serve hosting

Usage:
  %s login [url]
        Sign in to a platform. Pass its URL (e.g. loft login https://loft.example.com);
        the CLI discovers the OAuth settings from it and signs you in via the device flow.
  %s deploy [path] [name] [--force] [--url URL]
        Upload a folder to a site. Path defaults to the current dir; the site name is
        the 2nd argument (or --name), defaulting to the folder name.
  %s delete <name> [--force] [--url URL]
        Remove a site. Asks you to type the site name to confirm (irreversible).
  %s whoami [--url URL]
        Show the identity you are signed in as.
  %s version
        Print the CLI version.

The platform URL comes from --url, LOFT_URL, or the saved login. For CI, set LOFT_TOKEN
to a bearer token instead of running login.
`, brand, brand, brand, brand, brand, brand)
}

func ask(question string) string {
	fmt.Print(question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func parseArgs(argv []string) (positionals []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "--") {
			key := a[2:]
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				flags[key] = argv[i+1]
				i++
			} else {
				flags[key] = "true"
			}
		} else {
			positionals = append(positionals, a)
		}
	}
	return positionals, flags
}

var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeSite(s string) string {
	label := strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if label == "" {
		return "site"
	}
	return label
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}
