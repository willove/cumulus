package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// TestGoldenSetsFrozen pins the golden question sets backing the recorded
// scoreboards: they are checksummed; any edit to them must land together
// with a re-recorded scoreboard, never silently.
func TestGoldenSetsFrozen(t *testing.T) {
	manifest := map[string]string{
		"../../testdata/eval/chinalaw39.jsonl":  "e01207db094eda7b8bb268d006baf4593f3b60d68b87586c35a0196f799cebc7",
		"../../testdata/eval/cnlaw30.jsonl":     "92a68b50f72bd2e03b687c8637ef514ff3a7117385cdd947ca760bdbc4183d0f",
		"../../testdata/eval/chinalaw158.jsonl": "bf5833a825b64f8def7f633626fe1a91e83a1efb9b94bdb33415a19d92c734d0",
	}
	for path, want := range manifest {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if got != want {
			t.Fatalf("%s drifted: sha256=%s want %s — re-record the scoreboard together with the set", path, got, want)
		}
	}
}
