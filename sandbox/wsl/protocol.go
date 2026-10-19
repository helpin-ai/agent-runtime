// Package wsl contains the Windows-to-Linux sandbox protocol.
package wsl

import (
	"bytes"
	"encoding/json"
	"errors"
)

const MaxMessageBytes = 2 << 20

// Request contains execution scope only; credentials never cross this boundary.
type Request struct {
	Tool          string          `json:"tool"`
	Input         json.RawMessage `json:"input"`
	Root          string          `json:"root"`
	AppID         string          `json:"app_id"`
	AgentID       string          `json:"agent_id"`
	RunID         string          `json:"run_id"`
	LeaseID       string          `json:"lease_id"`
	Provider      string          `json:"provider"`
	ReadOnly      bool            `json:"read_only"`
	Preview       bool            `json:"preview"`
	PythonAllowed bool            `json:"python_allowed"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > MaxMessageBytes {
		return 0, errors.New("WSL response exceeds its limit")
	}
	return b.buffer.Write(data)
}
func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *boundedBuffer) String() string { return b.buffer.String() }
