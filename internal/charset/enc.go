package charset

import (
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// gbEncode is the inverse of the tier-2 decode, used by the package's own
// tests so they exercise the real conversion instead of hand-made invalid
// bytes. It is unexported on purpose: callers have no business producing
// GB18030 in this suite.
func gbEncode(s string) ([]byte, error) {
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), []byte(s))
	return out, err
}
