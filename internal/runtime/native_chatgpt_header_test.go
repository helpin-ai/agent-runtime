package runtime

import (
	"context"
	"fmt"
	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatGPTStreamFiltersKeepalivesWithoutSSEContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "event: keepalive\n\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	retries := 0
	provider, err := agenticopenai.NewResponsesModel(context.Background(), &agenticopenai.ResponsesConfig{APIKey: "test", Model: "test", BaseURL: server.URL, MaxRetries: &retries, HTTPClient: &http.Client{Transport: chatGPTTransport{base: http.DefaultTransport}}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := (EinoAgenticModelFactory{Model: provider, Provider: "openai_chatgpt"}).ResolveNativeModel(t.Context(), contextTestExec(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(t.Context(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "hello" {
		t.Fatalf("response=%+v", response)
	}
}
