package deep

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

func TestExpandWindowsGrowsAgainstBody(t *testing.T) {
	body := strings.Repeat("x", 40) + "CORE" + strings.Repeat("y", 40)
	src := source.New("t", "md", "", "bk", "zh", body, nil)
	kept := []mcs.Sample{{Source: src.ID, Start: 40, End: 44, Content: "CORE", Score: 8}}
	out := expandWindows(kept, []source.Source{src})
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].Start != 16 || out[0].End != 68 {
		t.Fatalf("span want [16,68) got [%d,%d)", out[0].Start, out[0].End)
	}
	if !strings.Contains(out[0].Content, "CORE") {
		t.Fatalf("expanded content must keep core: %q", out[0].Content)
	}
}
