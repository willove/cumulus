package main

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/fast"
)

// The UI reads the JSON of these structs by hand, in JavaScript, with no compiler
// on either side of the wire. Two fields drifted and nobody noticed:
//
//	TestPanel read result.answer.conf      — the tag is "confidence", so 置信度
//	                                         rendered 0% on every search
//	TestPanel read citations.refs[].score  — deep.Ref has no score at all (the
//	                                         per-window scores live on
//	                                         answer.samples[], mcs.Sample), so the
//	                                         「得分」 line could never appear
//
// Both looked like working UI. This test is the mechanism: it reads the ACTUAL
// struct tags via reflection and the ACTUAL property accesses out of the .vue
// source, so neither side can drift without the other being named. The browser
// gate cannot do this job — it asserts rendered text, and a field that renders as
// "0%" or silently doesn't render is still a rendered page.
//
// Deliberately narrow: only the plain-JSON /v1/search contract is struct-tagged.
// The SSE `done` payload is assembled as a map literal in searchapi.go, so it has
// no tags to reflect over and is not covered here.
func TestSearchFaceFieldNamesMatchGoTags(t *testing.T) {
	const view = "../../web/src/views/TestPanel.vue"
	raw, err := os.ReadFile(view)
	if err != nil {
		t.Skipf("UI sources absent (%s): %v", view, err)
	}
	src := stripComments(string(raw))

	// jsonNames collects the wire names a struct actually marshals under,
	// following one level of embedding-free fields (these structs are flat).
	jsonNames := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				name = f.Name
			}
			out[name] = true
		}
		return out
	}

	// accessed pulls every property name reached through a given accessor prefix,
	// in template and script alike: `result.foo`, `result?.foo`, `answer?.foo`.
	// In the <script> half the accessor is a Vue ref, so the chain carries a
	// `.value` step (`result.value?.citations`); consuming it here keeps
	// everything past it in scope. The explicit "value" filter is still needed
	// because RE2 backtracks: for `result.value = null` the optional group is
	// skipped and `value` itself gets captured. It is a reactivity artifact and
	// never a wire field, so dropping it cannot hide a contract mismatch.
	accessed := func(prefix string) []string {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(prefix) + `(?:\.value)?\??\.([A-Za-z_][A-Za-z0-9_]*)`)
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			if m[1] == "value" {
				continue
			}
			seen[m[1]] = true
		}
		var out []string
		for k := range seen {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	cases := []struct {
		accessor   string
		tags       map[string]bool
		structName string
	}{
		{"result", jsonNames(deep.Result{}), "deep.Result"},
		{"answer", jsonNames(fast.Answer{}), "fast.Answer"},
		{"r", jsonNames(deep.Ref{}), "deep.Ref"},
	}
	for _, tc := range cases {
		for _, field := range accessed(tc.accessor) {
			if !tc.tags[field] {
				known := make([]string, 0, len(tc.tags))
				for k := range tc.tags {
					known = append(known, k)
				}
				sort.Strings(known)
				t.Errorf("web/src/views/TestPanel.vue reads %s.%s, but %s has no such JSON field.\n"+
					"  It marshals: %s\n"+
					"  A field that does not exist renders as a plausible wrong value (0%%, blank)\n"+
					"  or as a v-if that is never true — not as an error.",
					tc.accessor, field, tc.structName, strings.Join(known, ", "))
			}
		}
	}

	// The positive half: the reflective check above would also pass if the panel
	// simply stopped reading anything, so name the two wire fields whose absence
	// was the original bug — 置信度 (answer.confidence) and the 结构标签 (ref.span).
	for _, want := range []string{"answer?.confidence", "r.span"} {
		if !strings.Contains(src, want) {
			t.Errorf("TestPanel.vue must read %s (the wire names behind 置信度 and the 结构标签)", want)
		}
	}
}

// stripComments removes HTML, JS line and JS block comments so that a comment
// NAMING a wrong field cannot fail the check. That is not hypothetical: the fix
// for the two bugs above is documented in TestPanel.vue as "reading r.score here
// rendered a line that could never appear", and the first version of this test
// failed on its own explanation.
//
// It is a lint, not a JS parser. A `//` inside a string literal would be cut
// too; that is acceptable because the strings in these views are API paths
// ("/v1/search") and UI copy, neither of which contains a double slash, and the
// failure mode if one ever appears is a missed check rather than a false alarm.
func stripComments(s string) string {
	s = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, " ")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
