package eval

import "testing"

func TestFreezeBindsItems(t *testing.T) {
	a := Freeze([]byte("q1\nq2\n"), nil, nil, 0)
	b := Freeze([]byte("q1\nq2\n"), nil, nil, 0)
	c := Freeze([]byte("q1\nq2\nq3\n"), nil, nil, 0)
	if a.ItemsSHA == "" || a.ItemsSHA != b.ItemsSHA {
		t.Fatalf("same items must hash equal: %q vs %q", a.ItemsSHA, b.ItemsSHA)
	}
	if a.ItemsSHA == c.ItemsSHA {
		t.Fatal("different items must hash differently")
	}
}
