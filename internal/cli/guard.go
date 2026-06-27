package cli

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Guardrails: make the common mistakes impossible rather than merely warned. Loft serves static
// files behind auth, so we ship only real web assets, keep files and the whole site within sane
// limits, and require an entry point. These catch `loft deploy .` from a project root (node_modules,
// a leaked .env), a stray huge video, or a folder that serves nothing.
const (
	maxFileBytes  = 25 * 1024 * 1024  // per file
	maxTotalBytes = 100 * 1024 * 1024 // per site
	maxFiles      = 2000
)

var (
	allowedExt = newSet(
		"html", "htm", "css", "js", "mjs", "cjs", "json", "map", "wasm", "webmanifest",
		"svg", "png", "jpg", "jpeg", "gif", "webp", "avif", "ico", "bmp",
		"woff", "woff2", "ttf", "otf", "eot",
		"txt", "xml", "csv", "pdf", "md",
		"mp4", "webm", "ogg", "mp3", "wav", "m4a",
	)
	blockedDirs = []string{"node_modules", ".git"}
	envFileRe   = regexp.MustCompile(`(^|/)\.env(\.|$)`)
)

type fileEntry struct {
	abs  string
	rel  string // posix path within the site
	size int64
}

func collect(localDir string) ([]fileEntry, error) {
	var out []fileEntry
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinks: only regular files inside the folder are deployed, so a link can't pull in
		// content from outside the tree (or an unreadable/secret target) on upload.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		out = append(out, fileEntry{abs: path, rel: filepath.ToSlash(rel), size: info.Size()})
		return nil
	})
	return out, err
}

// validate returns a single error listing every reason the folder can't be deployed, or nil.
func validate(entries []fileEntry) error {
	var problems []string
	add := func(s string) { problems = append(problems, s) }

	if len(entries) == 0 {
		add("the folder is empty — nothing to deploy")
	}
	if len(entries) > maxFiles {
		add(fmt.Sprintf("too many files (%d > %d) — deploy a build output, not a project root", len(entries), maxFiles))
	}
	if !hasRoot(entries, "index.html") {
		add("no index.html at the root — a site needs an entry point (try deploying your build dir, e.g. ./dist)")
	}
	if d := blockedDir(entries); d != "" {
		add(fmt.Sprintf("found %s/ — deploy a built site, not a project folder (exclude %s or deploy ./dist)", d, d))
	}
	if secrets := filterRel(entries, func(rel string) bool { return envFileRe.MatchString(rel) }); len(secrets) > 0 {
		add("refusing to upload secret files: " + strings.Join(secrets, ", "))
	}
	if bad := filterRel(entries, func(rel string) bool { return !allowedExt[extOf(rel)] }); len(bad) > 0 {
		add("disallowed file types (static web assets only): " + preview(bad))
	}
	var tooBig []string
	var total int64
	for _, e := range entries {
		total += e.size
		if e.size > maxFileBytes {
			tooBig = append(tooBig, fmt.Sprintf("%s (%s)", e.rel, mb(e.size)))
		}
	}
	if len(tooBig) > 0 {
		add(fmt.Sprintf("files over %s: %s", mb(maxFileBytes), preview(tooBig)))
	}
	if total > maxTotalBytes {
		add(fmt.Sprintf("site is too large: %s > %s", mb(total), mb(maxTotalBytes)))
	}

	if len(problems) > 0 {
		return fmt.Errorf("deploy blocked:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func hasRoot(entries []fileEntry, rel string) bool {
	for _, e := range entries {
		if e.rel == rel {
			return true
		}
	}
	return false
}

func blockedDir(entries []fileEntry) string {
	for _, d := range blockedDirs {
		for _, e := range entries {
			if e.rel == d || strings.HasPrefix(e.rel, d+"/") {
				return d
			}
		}
	}
	return ""
}

func filterRel(entries []fileEntry, pred func(string) bool) []string {
	var out []string
	for _, e := range entries {
		if pred(e.rel) {
			out = append(out, e.rel)
		}
	}
	return out
}

func extOf(p string) string {
	base := p[strings.LastIndex(p, "/")+1:]
	if dot := strings.LastIndex(base, "."); dot > 0 {
		return strings.ToLower(base[dot+1:])
	}
	return "" // ".env" / "LICENSE" → no ext
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024)) }

func preview(xs []string) string {
	if len(xs) > 10 {
		return strings.Join(xs[:10], ", ") + fmt.Sprintf(" …and %d more", len(xs)-10)
	}
	return strings.Join(xs, ", ")
}

func newSet(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}
