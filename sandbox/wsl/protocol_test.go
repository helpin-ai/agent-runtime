package wsl

import (
	"io"
	"strings"
	"testing"
)

func TestResponseBufferCannotBypassLimitViaCopy(t *testing.T) {
	var output boundedBuffer
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", MaxMessageBytes+1)), MaxMessageBytes+1))
	if err == nil || len(output.Bytes()) > MaxMessageBytes {
		t.Fatal("accepted an oversized helper response")
	}
}
