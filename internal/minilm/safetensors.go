// Package minilm runs the sentence-transformers model
// paraphrase-multilingual-MiniLM-L12-v2 (384-dim multilingual embeddings) in
// pure Go — safetensors weights + HF tokenizer spec + a hand-rolled BERT
// forward pass, stdlib only. It exists so the ask suite's vector L1 can speak
// the SAME vector space as Sirchmunk's semantic cache index without cgo,
// ONNX runtime, or a Python sidecar.
//
// Feasibility numbers (see ../db-works/docs/suites/ask/embed-notes.md): 118M params,
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
func (s *safetensors) tensor(name string) ([]float32, error) {
	t, ok := s.header[name]
	if !ok {
		return nil, fmt.Errorf("tensor %q not found", name)
	}
	if t.Dtype != "F32" {
		return nil, fmt.Errorf("tensor %q: dtype %s, want F32", name, t.Dtype)
	}
	off := s.dataStart + t.DataOffsets[0]
	n := (t.DataOffsets[1] - t.DataOffsets[0]) / 4
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

// matvecRowMajor computes y[o] = Σ_i x[i]·W[o·in+i] + b[o] (nn.Linear with
// weight [out,in] applied as x·Wᵀ), blocked over i for cache locality.
func matvecRowMajor(x []float32, w []float32, b []float32, out int, in int, y []float32) {
	for o := 0; o < out; o++ {
		var acc float32
		row := w[o*in : o*in+in]
		for i := 0; i < in; i++ {
			acc += x[i] * row[i]
		}
		y[o] = acc + b[o]
	}
}
