package sandbox

import (
	"fmt"
	"sync"
)

// headTail 保留输出的前一半和后一半，丢掉中间。与 Codex 的
// unified_exec/head_tail_buffer.rs 相同：开头通常是命令说明或第一个错误，
// 结尾通常是最终结果，二者都比中间重要。
type headTail struct {
	mu      sync.Mutex
	max     int
	head    []byte
	tail    []byte // 环形使用会更省，但输出上限只有几十 KB，直接拷贝足够
	omitted int
}

func newHeadTail(max int) *headTail { return &headTail{max: max} }

func (b *headTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	headBudget := b.max / 2
	if room := headBudget - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	tailBudget := b.max - headBudget
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - tailBudget; over > 0 {
		b.omitted += over
		b.tail = append(b.tail[:0:0], b.tail[over:]...)
	}
	return n, nil
}

// String 返回保留的内容；有省略时在中间插入一行说明。
func (b *headTail) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.omitted == 0 {
		return string(b.head) + string(b.tail), false
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", b.head, b.omitted, b.tail), true
}
