package llm

import (
	"testing"
)

// TestThinkingLevelValues pins the level strings — they're the wire format
// for both output_config.effort (MiniMax) and reasoning_effort (OpenAI).
func TestThinkingLevelValues(t *testing.T) {
	levels := map[ThinkingLevel]string{
		ThinkingLow: "low", ThinkingMedium: "medium", ThinkingHigh: "high",
		ThinkingXHigh: "xhigh", ThinkingMax: "max",
	}
	for level, want := range levels {
		if string(level) != want {
			t.Fatalf("level %q != %q", level, want)
		}
	}
}

// TestStageEffortEnvOverride pins: valid env values override the fallback;
// empty/garbage falls back; each stage reads its own variable.
func TestStageEffortEnvOverride(t *testing.T) {
	// Default fallback.
	if got := StageEffort("SYNTH", ThinkingHigh); got != ThinkingHigh {
		t.Fatalf("default: %q", got)
	}
	// Valid override.
	t.Setenv("CLUS_THINK_SYNTH", "low")
	if got := StageEffort("SYNTH", ThinkingHigh); got != ThinkingLow {
		t.Fatalf("override: %q", got)
	}
	// Garbage falls back.
	t.Setenv("CLUS_THINK_SYNTH", "garbage")
	if got := StageEffort("SYNTH", ThinkingHigh); got != ThinkingHigh {
		t.Fatalf("garbage: %q", got)
	}
	// Different stage, different variable.
	t.Setenv("CLUS_THINK_SCORE", "medium")
	if got := StageEffort("SCORE", ThinkingLow); got != ThinkingMedium {
		t.Fatalf("score override: %q", got)
	}
	// Unset stage keeps fallback.
	t.Setenv("CLUS_THINK_SCORE", "")
	if got := StageEffort("SCORE", ThinkingLow); got != ThinkingLow {
		t.Fatalf("unset: %q", got)
	}
}
