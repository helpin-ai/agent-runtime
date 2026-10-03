package memory

import (
	"github.com/tiktoken-go/tokenizer"
	"sync"
)

var tokenCodec = sync.OnceValue(func() tokenizer.Codec {
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		panic(err)
	}
	return codec
})

// Match Hindsight's default o200k_base budgets, including Unicode text.
func countTokens(text string) int {
	count, err := tokenCodec().Count(text)
	if err != nil {
		return len(text)
	} // Conservative if the tokenizer cannot process input.
	return count
}
