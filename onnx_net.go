package main

import (
	"errors"
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// onnxNet is a model whose inputs follow the data: each run makes its
// tensors and returns each output's values and shape.
type onnxNet struct {
	name string
	s    *ort.DynamicAdvancedSession
	outs int
}

func newONNX(name, path string, in, out []string) (*onnxNet, error) {
	s, err := withOptions(func(o *ort.SessionOptions) (*ort.DynamicAdvancedSession, error) {
		return ort.NewDynamicAdvancedSession(path, in, out, o)
	})
	if err != nil {
		return nil, fmt.Errorf("load the %s model: %w", name, err)
	}
	return &onnxNet{name: name, s: s, outs: len(out)}, nil
}

// onnxIn is one float input: its shape and values.
type onnxIn struct {
	shape []int64
	data  []float32
}

// onnxOut is one output, copied out of the runtime.
type onnxOut struct {
	shape []int64
	data  []float32
}

func (n *onnxNet) run(inputs ...onnxIn) (_ []onnxOut, err error) {
	ins := make([]ort.Value, 0, len(inputs))
	outs := make([]ort.Value, n.outs)
	defer func() {
		for _, v := range append(ins, outs...) {
			if v != nil {
				err = errors.Join(err, v.Destroy())
			}
		}
	}()
	for _, in := range inputs {
		t, err := ort.NewTensor(ort.NewShape(in.shape...), in.data)
		if err != nil {
			return nil, err
		}
		ins = append(ins, t)
	}
	if err := n.s.Run(ins, outs); err != nil {
		return nil, fmt.Errorf("%s model: %w", n.name, err)
	}
	res := make([]onnxOut, len(outs))
	for i, v := range outs {
		t, ok := v.(*ort.Tensor[float32])
		if !ok {
			return nil, fmt.Errorf("the %s model returned %T, not float32", n.name, v)
		}
		res[i] = onnxOut{shape: append([]int64(nil), t.GetShape()...), data: append([]float32(nil), t.GetData()...)}
	}
	return res, nil
}

func (n *onnxNet) close() error { return n.s.Destroy() }
