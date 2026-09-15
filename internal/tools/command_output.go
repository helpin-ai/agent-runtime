package tools

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

const commandOutputLimit = 50_000

// commandOutput retains a bounded tail even while a command is still running.
type commandOutput struct {
	mu    sync.Mutex
	tail  []byte
	total int64
}

func (b *commandOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if n >= commandOutputLimit {
		b.tail = append(b.tail[:0], p[n-commandOutputLimit:]...)
	} else {
		if excess := len(b.tail) + n - commandOutputLimit; excess > 0 {
			copy(b.tail, b.tail[excess:])
			b.tail = b.tail[:len(b.tail)-excess]
		}
		b.tail = append(b.tail, p...)
	}
	return n, nil
}

func (b *commandOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := b.tail
	if b.total > int64(len(tail)) {
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
		return fmt.Sprintf("[Output truncated; showing final %d of %d bytes]\n", len(tail), b.total) + strings.ToValidUTF8(string(tail), "�")
	}
	return strings.ToValidUTF8(string(tail), "�")
}
