package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestProviderReasoningSurvivesCheckpointWithoutPublicExposure(t *testing.T) {
	source := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.Reasoning{Text: "summary", Signature: "opaque-first"}),
		schema.NewContentBlock(&schema.Reasoning{Signature: "opaque-second"}),
	}}
	source.ContentBlocks[0].Extra = map[string]any{"item_id": "reasoning-item"}
	source.ContentBlocks[0].StreamingMeta = &schema.StreamingMeta{Index: 0}
	native := withAgenticReasoning(NativeMessage{Role: "assistant", Content: "answer"}, source, "openai_chatgpt")
	raw, err := json.Marshal(nativeCheckpoint{Format: 1, Messages: []NativeMessage{native}})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint nativeCheckpoint
	if err = json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	messages, err := nativeMessagesForProvider("", checkpoint.Messages, nativeToolNameMapper{}, "openai_chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	blocks := messages[0].ContentBlocks
	if len(blocks) != 3 || blocks[0].Reasoning.Signature != "opaque-first" || blocks[1].Reasoning.Signature != "opaque-second" {
		t.Fatalf("reasoning lost or reordered: %+v", blocks)
	}
	if blocks[0].Extra["item_id"] != "reasoning-item" || blocks[0].StreamingMeta == nil {
		t.Fatal("reasoning metadata lost")
	}
	public, err := json.Marshal(publicNativeMessages(checkpoint.Messages))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "opaque-") {
		t.Fatal("provider state exposed")
	}
	if _, err = nativeMessagesForProvider("", checkpoint.Messages, nativeToolNameMapper{}, "openai"); err == nil {
		t.Fatal("cross-provider checkpoint accepted")
	}
	checkpoint.Messages[0].ProviderState.Reasoning = []byte(`{"bad`)
	if _, err = nativeMessagesForProvider("", checkpoint.Messages, nativeToolNameMapper{}, "openai_chatgpt"); err == nil {
		t.Fatal("corrupt checkpoint silently dropped")
	}
}
