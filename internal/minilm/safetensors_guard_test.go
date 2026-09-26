package minilm

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The weights file arrives over the network; i64Row used to index
// DataOffsets[0]/[1] and slice the data section without any validation —
// the exact panics tensor() was hardened against — so the first query on a
// malformed download crashed the process instead of returning an error.
func writeSafetensors(t *testing.T, header string, payload []byte) *safetensors {
	t.Helper()
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(len(header)))
	buf = append(append(buf, header...), payload...)
	path := filepath.Join(t.TempDir(), "guard.safetensors")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openSafetensors(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestI64RowRejectsMalformedOffsets(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"one entry", `{"p":{"dtype":"I64","shape":[2],"data_offsets":[0]}}`},
		{"end before begin", `{"p":{"dtype":"I64","shape":[2],"data_offsets":[16,0]}}`},
		{"overrun", `{"p":{"dtype":"I64","shape":[2],"data_offsets":[0,64]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := writeSafetensors(t, tc.header, make([]byte, 16))
			if _, err := s.i64Row("p"); err == nil || !strings.Contains(err.Error(), "tensor") {
				t.Fatalf("malformed offsets must error, got %v", err)
			}
		})
	}
}

func TestI64RowReadsValidBuffer(t *testing.T) {
	s := writeSafetensors(t, `{"p":{"dtype":"I64","shape":[2],"data_offsets":[0,16]}}`, []byte{
		1, 0, 0, 0, 0, 0, 0, 0,
		2, 0, 0, 0, 0, 0, 0, 0,
	})
	row, err := s.i64Row("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(row) != 2 || row[0] != 1 || row[1] != 2 {
		t.Fatalf("row = %v, want [1 2]", row)
	}
}
