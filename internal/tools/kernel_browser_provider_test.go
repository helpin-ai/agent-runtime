package tools

import (
	"errors"
	"net/http"
	"testing"

	kernel "github.com/kernel/kernel-go-sdk"
)

func TestKernelCreditUnavailableClassification(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{name: "payment required", statusCode: http.StatusPaymentRequired, body: `{}`, want: true},
		{name: "credit detail", statusCode: http.StatusForbidden, body: `{"error":{"message":"Insufficient credits"}}`, want: true},
		{name: "billing detail", statusCode: http.StatusUnprocessableEntity, body: `{"detail":"billing balance exhausted"}`, want: true},
		{name: "invalid key", statusCode: http.StatusUnauthorized, body: `{"error":"invalid api key"}`},
		{name: "rate limit", statusCode: http.StatusTooManyRequests, body: `{"error":"concurrency limit exceeded"}`},
		{name: "server error", statusCode: http.StatusInternalServerError, body: `{"error":"credit service failed"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := &kernel.Error{StatusCode: tt.statusCode}
			if err := apiErr.UnmarshalJSON([]byte(tt.body)); err != nil {
				t.Fatalf("unmarshal Kernel error: %v", err)
			}
			wrapped := safeKernelOperationError("create Kernel browser", apiErr)
			if got := kernelCreditUnavailable(wrapped); got != tt.want {
				t.Fatalf("kernelCreditUnavailable() = %t, want %t", got, tt.want)
			}
			var operationErr *kernelOperationError
			if !errors.As(wrapped, &operationErr) || operationErr.statusCode != tt.statusCode {
				t.Fatalf("safe error = %#v", wrapped)
			}
		})
	}
}
