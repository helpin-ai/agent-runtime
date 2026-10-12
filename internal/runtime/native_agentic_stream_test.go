package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/schema"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func indexedAgenticCall(index int, id, name, arguments string) *schema.ContentBlock {
	return schema.NewContentBlockChunk(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: arguments}, &schema.StreamingMeta{Index: index})
}

func TestAgenticStreamKeepsInterleavedParallelQueries(t *testing.T) {
	message := func(blocks ...*schema.ContentBlock) *schema.AgenticMessage {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: blocks}
	}
	model := einoAgenticNativeModel{provider: "openai", model: &fakeEinoAgenticModel{streamChunks: []*schema.AgenticMessage{
		message(indexedAgenticCall(1, "first", "web_search", ""), indexedAgenticCall(5, "second", "web_search", ""), indexedAgenticCall(9, "third", "web_search", "")),
		message(indexedAgenticCall(5, "", "", `{"query":"second`), indexedAgenticCall(1, "", "", `{"query":"first`)),
		message(indexedAgenticCall(9, "", "", `{"query":"third query"}`)),
		message(indexedAgenticCall(1, "", "", " "), indexedAgenticCall(5, "", "", " ")),
		message(indexedAgenticCall(5, "", "", `query"}`), indexedAgenticCall(1, "", "", `query"}`)),
		message(indexedAgenticCall(9, "third", "web_search", ""), indexedAgenticCall(1, "first", "web_search", ""), indexedAgenticCall(5, "second", "web_search", "")),
	}}}
	stream, err := model.Stream(context.Background(), NativeModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []NativeModelResponse
	liveArguments := map[string]string{}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, *chunk)
		for _, call := range nativeRawToolCallBlocks(chunk.Message) {
			if call.ToolCallID == "" || call.ToolName != "web_search" {
				t.Fatalf("argument identity missing: %+v", call)
			}
			if string(call.Input) != "{}" {
				liveArguments[call.ToolCallID] += string(call.Input)
			}
		}
	}
	result := concatNativeModelStreamResponses(chunks)
	calls := nativeRawToolCallBlocks(result.Message)
	if len(calls) != 3 {
		t.Fatalf("calls=%+v", calls)
	}
	for i, id := range []string{"first", "second", "third"} {
		var input struct {
			Query string `json:"query"`
		}
		if calls[i].ToolCallID != id || json.Unmarshal(calls[i].Input, &input) != nil || input.Query != id+" query" {
			t.Fatalf("query corrupted: %+v", calls[i])
		}
		if liveArguments[id] != string(calls[i].Input) {
			t.Fatalf("live arguments=%q final=%s", liveArguments[id], calls[i].Input)
		}
	}
}

func TestAgenticStreamRejectsUnidentifiedOrReassignedArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []*schema.ContentBlock
		want   string
	}{
		{"missing initial ID", []*schema.ContentBlock{indexedAgenticCall(4, "", "", `{"query":"wrong call"}`)}, "omitted its initial ID"},
		{"changed ID", []*schema.ContentBlock{indexedAgenticCall(4, "first", "web_search", ""), indexedAgenticCall(4, "second", "web_search", `{"query":"wrong call"}`)}, "changed ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &einoAgenticNativeModelStream{reader: schema.StreamReaderFromArray([]*schema.AgenticMessage{{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: tc.blocks}})}
			defer stream.Close()
			_, err := stream.Recv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestChatGPTResponsesKeepsParallelSearchArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event map[string]any) {
			raw, err := json.Marshal(event)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		items := []map[string]any{
			{"id": "fc_a", "type": "function_call", "call_id": "call_a", "name": "web_search", "arguments": "", "status": "in_progress"},
			{"id": "fc_b", "type": "function_call", "call_id": "call_b", "name": "web_search", "arguments": "", "status": "in_progress"},
		}
		for i, item := range items {
			send(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
		}
		for _, part := range []struct {
			index int
			args  string
		}{{1, `{"query":"second`}, {0, `{"query":"first`}, {0, " "}, {1, " "}, {1, `query"}`}, {0, `query"}`}} {
			send(map[string]any{"type": "response.function_call_arguments.delta", "output_index": part.index, "item_id": items[part.index]["id"], "delta": part.args})
		}
		for i, item := range items {
			query := []string{"first query", "second query"}[i]
			args, _ := json.Marshal(map[string]string{"query": query})
			item["arguments"] = string(args)
			item["status"] = "completed"
			send(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_parallel", "object": "response", "status": "completed", "model": "test", "output": items}})
	}))
	defer server.Close()
	retries := 0
	provider, err := agenticopenai.NewResponsesModel(t.Context(), &agenticopenai.ResponsesConfig{APIKey: "test", Model: "test", BaseURL: server.URL, MaxRetries: &retries, HTTPClient: &http.Client{Transport: chatGPTTransport{base: http.DefaultTransport}}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := (EinoAgenticModelFactory{Model: provider, Provider: "openai_chatgpt"}).ResolveNativeModel(t.Context(), contextTestExec(t), []tools.Definition{{Name: "web_search", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(t.Context(), NativeModelRequest{SystemPrompt: "Search", Messages: []NativeMessage{{Role: "user", Content: "Find two stories"}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := nativeRawToolCallBlocks(response.Message)
	if len(calls) != 2 {
		t.Fatalf("calls=%+v", calls)
	}
	for i, want := range []string{"first query", "second query"} {
		var input struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(calls[i].Input, &input) != nil || input.Query != want || calls[i].ToolCallID != []string{"call_a", "call_b"}[i] {
			t.Fatalf("query lost through Responses parser: %+v", calls[i])
		}
	}
}
