package deep

import (
	"testing"

	"github.com/willove/cumulus/internal/fast"
)

// C-1: SkipBelow and EscalateBelow used to be two independent literals, both
// 0.35, each with a comment claiming they could not desynchronise. They could:
// with CLUS_ESCALATE_BELOW unset, editing one and not the other moved the FAST
// skip floor without moving the DEEP escalation line — the FAST package cannot
// import deep (deep already imports fast), so the only cycle-free direction is
// deep deriving from fast.
//
// These assertions fail if a second literal ever comes back.

func TestEscalateBelowIsNotASecondLiteral(t *testing.T) {
	if EscalateBelow != fast.SkipBelow {
		t.Fatalf("EscalateBelow = %v but fast.SkipBelow = %v — the two are independent "+
			"again, so the FAST skip floor and the DEEP escalation line can drift "+
			"apart whenever CLUS_ESCALATE_BELOW is unset", EscalateBelow, fast.SkipBelow)
	}
}

// The shared knob must also behave identically on both sides, including the
// out-of-bounds and malformed cases. A difference here would mean the FAST
// skip flag and the DEEP escalation line disagree under operator override.
func TestSkipAndEscalateLinesAgreeUnderTheKnob(t *testing.T) {
	cases := []struct {
		name string
		set  string
	}{
		{"unset", ""},
		{"valid low", "0.20"},
		{"valid high", "0.80"},
		{"negative is rejected", "-0.1"},
		{"above the ceiling is rejected", "0.99"},
		{"malformed is rejected", "not-a-number"},
		{"whitespace padded", "  0.55  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set == "" {
				t.Setenv("CLUS_ESCALATE_BELOW", "")
			} else {
				t.Setenv("CLUS_ESCALATE_BELOW", tc.set)
			}
			gotFast, gotDeep := fast.SkipBelowLine(), escalateBelowLine()
			if gotFast != gotDeep {
				t.Fatalf("same env, different lines: fast.SkipBelowLine()=%v escalateBelowLine()=%v "+
					"(env=%q) — a skipped answer is itself an escalation trigger, so these "+
					"must never disagree", gotFast, gotDeep, tc.set)
			}
		})
	}
}

// A rejected override must land on the shared default, not on each side's own
// fallback — that is the specific case the duplicated literals used to break.
func TestRejectedOverrideFallsBackToTheSharedDefault(t *testing.T) {
	t.Setenv("CLUS_ESCALATE_BELOW", "0.99") // above the 0.95 ceiling
	if got := escalateBelowLine(); got != fast.SkipBelow {
		t.Fatalf("escalateBelowLine() = %v for a rejected override, want the shared "+
			"default %v", got, fast.SkipBelow)
	}
}
