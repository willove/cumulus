package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmDownload(t *testing.T) {
	cases := map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, "": false, "n\n": false, "\n": false,
	}
	for in, want := range cases {
		var out bytes.Buffer
		if got := confirmDownload(strings.NewReader(in), &out, "download?"); got != want {
			t.Fatalf("confirmDownload(%q) = %v, want %v", in, got, want)
		}
		if !strings.Contains(out.String(), "download?") {
			t.Fatalf("prompt not shown: %q", out.String())
		}
	}
}

func TestVerifyModelWithoutWeights(t *testing.T) {
	if _, err := verifyModel(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "model install") {
		t.Fatalf("absent weights must point at the installer: %v", err)
	}
	// A directory with a bogus safetensors must surface as an error (not a panic).
	dir := t.TempDir()
	if err := writeFile(dir, "model.safetensors", "not-a-safetensors"); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyModel(context.Background(), dir); err == nil {
		t.Fatalf("corrupt weights must fail verify")
	}
}

func TestCollectModelStatus(t *testing.T) {
	dir := t.TempDir()
	st := collectModelStatus(dir)
	if st.Installed || st.Dims != 384 || len(st.Files) != 0 {
		t.Fatalf("empty dir status: %+v", st)
	}
	if err := writeFile(dir, "model.safetensors", "x"); err != nil {
		t.Fatal(err)
	}
	st = collectModelStatus(dir)
	if !st.Installed || len(st.Files) != 1 || st.Files[0].Size != 1 {
		t.Fatalf("status after drop: %+v", st)
	}
}

func writeFile(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}
