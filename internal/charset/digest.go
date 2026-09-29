package charset

import (
	"crypto/sha256"
	"encoding/hex"
)

// digestHex is sha256 of the raw input bytes. The suite's source.Digest lives
// in internal/source and covers the CONVERTED + normalized body; this one
// covers the file as it was on disk. Keeping both is what makes the conversion
// auditable after the fact: "which file produced this text" stays answerable
// even though the text itself is no longer byte-identical to the file.
func digestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
