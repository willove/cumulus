// Package minilm runs the sentence-transformers model
// paraphrase-multilingual-MiniLM-L12-v2 (384-dim multilingual embeddings) in
// pure Go — safetensors weights + HF tokenizer spec + a hand-rolled BERT
// forward pass, stdlib only. It exists so the cumulus-cluster suite's vector L1 can speak
// the SAME vector space as Sirchmunk's semantic cache index without cgo,
// ONNX runtime, or a Python sidecar.
//
// Feasibility numbers: 118M params,
// ~5.4 GFLOP per 128-token sequence on 12 CPU threads.
package minilm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"
)

// safetensors is a read-only view over an mmapped safetensors file.
type safetensors struct {
	data      []byte // whole file
	dataStart int    // absolute offset of the data section (8 + header length)
	header    map[string]stTensor
}

type stTensor struct {
	Dtype       string `json:"dtype"`
	Shape       []int  `json:"shape"`
	DataOffsets []int  `json:"data_offsets"`
}

// openSafetensors mmaps path and parses the JSON header.
func openSafetensors(path string) (*safetensors, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("%s: too short", path)
	}
	hlen := binary.LittleEndian.Uint64(data[:8])
	if uint64(len(data)) < 8+hlen {
		return nil, fmt.Errorf("%s: header overruns file", path)
	}
	var header map[string]stTensor
	if err := json.Unmarshal(data[8:8+hlen], &header); err != nil {
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	return &safetensors{data: data, dataStart: 8 + int(hlen), header: header}, nil
}

// tensor returns the raw F32 payload of one tensor. Weights in this model are
// all F32; offsets are 8-aligned per the safetensors spec, so an unsafe view
// avoids copying ~449MB.
//
// The offsets are validated BEFORE the unsafe view is built. The weights arrive
// over the network from ModelScope, so a truncated or malformed header must
// surface as an error - not as an index panic on DataOffsets[1], and not as a
// SIGSEGV from an unsafe.Slice that runs past the mmap.
func (s *safetensors) tensor(name string) ([]float32, error) {
	t, ok := s.header[name]
	if !ok {
		return nil, fmt.Errorf("tensor %q not found", name)
	}
	if t.Dtype != "F32" {
		return nil, fmt.Errorf("tensor %q: dtype %s, want F32", name, t.Dtype)
	}
	if len(t.DataOffsets) < 2 {
		return nil, fmt.Errorf("tensor %q: data_offsets needs 2 entries, got %d", name, len(t.DataOffsets))
	}
	begin, end := t.DataOffsets[0], t.DataOffsets[1]
	if begin < 0 || end < begin {
		return nil, fmt.Errorf("tensor %q: bad data_offsets [%d,%d]", name, begin, end)
	}
	if s.dataStart+begin < 0 || end > len(s.data)-s.dataStart {
		return nil, fmt.Errorf("tensor %q: offsets [%d,%d) overrun the %d-byte data section",
			name, begin, end, len(s.data)-s.dataStart)
	}
	n := (end - begin) / 4
	if n == 0 {
		return nil, fmt.Errorf("tensor %q: empty payload", name)
	}
	off := s.dataStart + begin
	return unsafe.Slice((*float32)(unsafe.Pointer(&s.data[off])), n), nil
}

// i64Row reads int64 values (position_ids buffer) for spec verification.
func (s *safetensors) i64Row(name string) ([]int64, error) {
	t, ok := s.header[name]
	if !ok {
		return nil, fmt.Errorf("tensor %q not found", name)
	}
	if t.Dtype != "I64" {
		return nil, fmt.Errorf("tensor %q: dtype %s, want I64", name, t.Dtype)
	}
	off := s.dataStart + t.DataOffsets[0]
	n := (t.DataOffsets[1] - t.DataOffsets[0]) / 8
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(s.data[off+i*8:]))
	}
	return out, nil
}

// gelu is the exact erf-based GELU BERT uses (hidden_act "gelu").
func gelu(x float32) float32 {
	return 0.5 * x * (1 + float32(math.Erf(float64(x)/math.Sqrt2)))
}

// layerNorm applies (x-μ)/√(σ²+eps)·w+b over one vector.
func layerNorm(x, w, b []float32, eps float32) {
	var mean, varr float64
	for _, v := range x {
		mean += float64(v)
	}
	mean /= float64(len(x))
	for _, v := range x {
		d := float64(v) - mean
		varr += d * d
	}
	varr /= float64(len(x))
	std := float32(math.Sqrt(varr + float64(eps)))
	for i := range x {
		x[i] = (x[i]-float32(mean))/std*w[i] + b[i]
	}
}
