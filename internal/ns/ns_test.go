package ns

import "testing"

func TestCollDefaultLibraryStaysBare(t *testing.T) {
	if got := Coll("", "ask_sources"); got != "ask_sources" {
		t.Fatalf("default library: got %q", got)
	}
}

func TestCollComposesCompositeIdentity(t *testing.T) {
	if got := Coll("tenant_a", "ask_clusters"); got != "tenant_a:ask_clusters" {
		t.Fatalf("composite: got %q", got)
	}
}

func TestKVScopedAndBare(t *testing.T) {
	if got := KV("", "ask:job:x"); got != "ask:job:x" {
		t.Fatalf("bare kv: got %q", got)
	}
	if got := KV("t1", "ask:job:x"); got != "ns:t1:ask:job:x" {
		t.Fatalf("scoped kv: got %q", got)
	}
}

func TestValidateAcceptsLegalSegments(t *testing.T) {
	for _, ok := range []string{"", "a", "tenant_a", "tenant-a", "tenant.a", "T9", "x_9"} {
		if err := Validate(ok); err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", ok, err)
		}
	}
}

func TestValidateRejectsEngineIllegalSegments(t *testing.T) {
	for _, bad := range []string{
		"_lead",   // engine reserve: '_' only in the default library
		".lead",   // leading '.'
		"-lead",   // leading '-'
		"a:b",     // second colon would make the identity triple-composite
		"a b",     // space
		"a/b",     // path byte
		"tenanté", // non-ASCII
	} {
		if err := Validate(bad); err == nil {
			t.Fatalf("Validate(%q) = nil, want error", bad)
		}
	}
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	if err := Validate(string(long)); err == nil {
		t.Fatal("Validate(129 bytes) = nil, want error")
	}
}
