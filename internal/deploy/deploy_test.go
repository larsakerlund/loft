package deploy

import (
	"bytes"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"
)

// buildForm writes a deploy multipart body: the site field first (the server requires it before the
// files), then each path->content as a "files" part whose multipart filename carries the full path.
func buildForm(t *testing.T, site string, files map[string]string) *multipart.Reader {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("site", site); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return multipart.NewReader(&buf, mw.Boundary())
}

// Regression for the RFC-7578 flattening bug: a build with subdirectories must stage with its
// directory structure intact. part.FileName() applies filepath.Base, which would land every file in
// the site root and break every reference under /assets/*.
func TestStagePreservesNestedPaths(t *testing.T) {
	staging := t.TempDir()
	files := map[string]string{
		"index.html":                "<h1>hi</h1>",
		"assets/index-abc.js":       "console.log(1)",
		"assets/modules/vue-xyz.js": "/* vue */",
	}
	mr := buildForm(t, "demo", files)

	out, uerr := stage(mr, staging)
	if uerr != nil {
		t.Fatalf("stage failed: %+v", uerr)
	}
	if out.site != "demo" {
		t.Fatalf("site = %q, want demo", out.site)
	}
	if out.files != len(files) {
		t.Fatalf("files = %d, want %d", out.files, len(files))
	}

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("expected staged file %q: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("staged %q = %q, want %q", rel, got, want)
		}
	}
}

// Path traversal in a part filename must be neutralized: the escaping segments are dropped and the
// file stays inside the staging dir. (cleanRelPath strips ".."; within() is the backstop.)
func TestStageRejectsTraversal(t *testing.T) {
	staging := t.TempDir()
	mr := buildForm(t, "demo", map[string]string{
		"index.html":      "<h1>hi</h1>",
		"../../escape.js": "pwned",
	})

	if _, uerr := stage(mr, staging); uerr != nil {
		t.Fatalf("stage failed: %+v", uerr)
	}
	// Nothing may be written outside the staging dir.
	if _, err := os.Stat(filepath.Join(filepath.Dir(staging), "escape.js")); !os.IsNotExist(err) {
		t.Fatalf("traversal escaped staging: %v", err)
	}
	// The sanitized basename stays inside staging.
	if _, err := os.Stat(filepath.Join(staging, "escape.js")); err != nil {
		t.Fatalf("expected sanitized file inside staging: %v", err)
	}
}
