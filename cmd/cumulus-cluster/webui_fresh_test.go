package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The embedded UI is a COMMITTED build artifact: webui.go's go:embed has to find
// cmd/cumulus-cluster/web/dist at compile time, and the appliance serves its own
// workbench with zero Node at runtime. Nothing enforced that the artifact matches
// web/src. It drifted once already — dist at 2124b76 (2026-10-01 05:59) against
// src at f4560a2 (2026-10-01 15:44) — and the one gate that could have caught it,
// `make browser-check`, exits 2 for a real failure because that is make's own
// error code, while the script documents 2 as "environment absent". A red gate
// that reads as a skip is worse than no gate.
//
// This test is the mechanism, and it deliberately needs NO Node so it runs inside
// `make check`: it hashes the UI sources and compares against the stamp written
// into web/BUILD-STAMP at build time. There is one hash implementation, shared by
// the checker and the writer — WEB_STAMP_UPDATE=1 flips this test from comparing
// to writing, which is what `make web` invokes after `npm run build`. Two
// implementations of the same hash (one in Go, one in shell) is how the stamp
// would come to mean nothing.
//
// The stamp lives BESIDE dist rather than inside it because vite runs with
// emptyOutDir and would delete it on the next build.

// webSrcDir is the UI source tree, relative to this package.
const webSrcDir = "../../web"

// webStampPath is where `make web` records the hash of the sources it built from.
const webStampPath = "web/BUILD-STAMP"

// stampSkipDirs are excluded from the hash: they are not inputs to the build.
// node_modules is dependencies (pinned by package-lock.json, which IS hashed),
// .mimosa is a local tool's scratch state and is gitignored.
var stampSkipDirs = map[string]bool{"node_modules": true, "dist": true, ".mimosa": true, ".git": true}

// webSourcesHash fingerprints every file that feeds the bundle. Paths are hashed
// alongside contents and sorted, so renaming a file changes the stamp: a rename
// is a source change even when no byte of content differs.
func webSourcesHash(t *testing.T, root string) string {
	t.Helper()
	type entry struct{ rel, body string }
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if stampSkipDirs[d.Name()] && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".DS_Store" {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		entries = append(entries, entry{filepath.ToSlash(rel), string(raw)})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s contains no files — nothing was hashed, so the stamp would be vacuous", root)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	fmt.Fprintf(h, "cumulus-web-src-v1 %d files\n", len(entries))
	for _, e := range entries {
		// Length-prefixed so a file whose content ends where another's name
		// begins cannot collide with a different split of the same bytes.
		fmt.Fprintf(h, "%d:%s\n%d:", len(e.rel), e.rel, len(e.body))
		h.Write([]byte(e.body))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestWebDistMatchesWebSrc(t *testing.T) {
	if _, err := os.Stat(webSrcDir); err != nil {
		t.Skipf("UI sources absent at %s (building from a source tarball without web/): %v", webSrcDir, err)
	}
	got := webSourcesHash(t, webSrcDir)

	if os.Getenv("WEB_STAMP_UPDATE") == "1" {
		body := got + "\n"
		if err := os.WriteFile(webStampPath, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", webStampPath, err)
		}
		t.Logf("wrote %s = %s", webStampPath, got)
		return
	}

	raw, err := os.ReadFile(webStampPath)
	if err != nil {
		t.Fatalf("reading %s: %v\n"+
			"The embedded UI has no build stamp, so nothing can tell whether web/dist was\n"+
			"built from the web/src you are looking at. Run: make web\n"+
			"(that is `cd web && npm run build` followed by WEB_STAMP_UPDATE=1 on this test,\n"+
			"which is the only writer of the stamp — the hash has exactly one implementation.)",
			webStampPath, err)
	}
	want := strings.TrimSpace(string(raw))
	if want != got {
		t.Fatalf("web/dist is STALE: it was built from web/src at %s, but the sources now hash to %s.\n"+
			"The binary embeds web/dist, so the UI it serves is not the UI in the repository —\n"+
			"editing web/src changes nothing a user sees until the artifact is rebuilt.\n"+
			"Run: make web   (then commit cmd/cumulus-cluster/web/dist together with web/src)",
			want, got)
	}
}
