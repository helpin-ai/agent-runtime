package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRequestToolsListTranslatesGatewayTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/runs/run-1/tools" || r.URL.Query().Get("app_id") != "app-a" {
			t.Fatalf("unexpected request path: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[{"name":"search","description":"Search","input_schema":{"type":"object"}}]}`))
	}))
	defer server.Close()

	id := json.RawMessage(`1`)
	resp := handleRequest(server.Client(), server.URL, "app-a", "run-1", "secret", rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/list",
	})

	if resp.Error != nil {
		t.Fatalf("expected success, got %#v", resp.Error)
	}
	payload, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(payload), `"inputSchema"`) || !strings.Contains(string(payload), `"search"`) {
		t.Fatalf("unexpected response payload: %s", string(payload))
	}
}

func TestReadMCPMessageSupportsContentLength(t *testing.T) {
	reader := strings.NewReader("Content-Length: 15\r\n\r\n{\"jsonrpc\":\"2\"}")
	msg, format, err := readMCPMessage(bufio.NewReader(reader))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if format != frameContentLength || string(msg) != `{"jsonrpc":"2"}` {
		t.Fatalf("unexpected message %q format %s", string(msg), format)
	}
}
