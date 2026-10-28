package agent

import (
	"context"
	"slices"
	"sync"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// Background 管理一个会话的后台压缩：上下文越过一个比自动压缩阈值低的
// 水位时，用当前模型历史的快照在后台提前生成摘要；之后需要压缩时直接采用
// 这份摘要，不用让用户再等一次摘要请求。对应 Codex 的
// core/src/session/async_compaction.rs。
//
// 一个会话一个 Background，跨轮次存在（轮末启动的请求可以在用户输入下一句
// 话时完成）。同一时间最多一个后台请求；每一代历史（两次压缩之间）只尝试
// 一次，失败了也不重试，与 Codex 的 last_attempt 相同。
type Background struct {
	provider llm.Provider
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu sync.Mutex
	// gen 是历史的代数，每次安装压缩结果（前台或后台）加一。
	gen int
	// tried 是已经发起过后台请求的那一代；-1 表示还没有。
	tried   int
	pending *candidate
	closed  bool
}

// candidate 是一次后台压缩：prefix 是发起时的模型历史快照，摘要只覆盖它。
type candidate struct {
	gen     int
	prefix  []protocol.Item
	done    chan struct{}
	summary string
	usage   protocol.Usage
	err     error
}

// NewBackground 返回使用 p 生成摘要的后台压缩器。ctx 结束（会话关闭）时
// 进行中的请求被取消。
func NewBackground(ctx context.Context, p llm.Provider) *Background {
	ctx, cancel := context.WithCancel(ctx)
	return &Background{provider: p, ctx: ctx, cancel: cancel, tried: -1}
}

// Close 取消进行中的请求并等待它结束。之后不再发起新的请求。
func (b *Background) Close() {
	b.mu.Lock()
	b.closed = true
	b.pending = nil
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

// Wait 等待进行中的后台请求结束，结果仍可被采用。
func (b *Background) Wait() {
	b.wg.Wait()
}

// start 为 hist 发起后台压缩；这一代已经尝试过时什么都不做。
func (b *Background) start(instructions string, hist []protocol.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.provider == nil || b.tried == b.gen || len(hist) == 0 {
		return
	}
	c := &candidate{gen: b.gen, prefix: slices.Clone(hist), done: make(chan struct{})}
	b.tried = b.gen
	b.pending = c
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(c.done)
		c.summary, c.usage, c.err = summarize(b.ctx, b.provider, instructions, c.prefix, nil)
	}()
}

// take 返回可以用在 hist 上的结果并把它从等待中取走；没有时返回 nil。
// 结果必须属于当前这一代，并且它的快照仍是 hist 的前缀。wait 为 true 时
// 等待进行中的请求，ctx 结束则放弃等待（请求本身继续，之后仍可采用）。
func (b *Background) take(ctx context.Context, hist []protocol.Item, wait bool) *candidate {
	b.mu.Lock()
	c := b.pending
	if c == nil || c.gen != b.gen {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	select {
	case <-c.done:
	default:
		if !wait {
			return nil
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending != c || c.gen != b.gen {
		return nil
	}
	b.pending = nil
	if c.err != nil || !isPrefix(c.prefix, hist) {
		return nil
	}
	return c
}

// advance 进入新的一代历史：丢弃旧的结果，允许再发起一次后台请求。进行中
// 的旧请求不取消，它完成后会因为代数不符被丢弃。
func (b *Background) advance() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gen++
	b.pending = nil
}

// isPrefix 按条目 id 判断 prefix 是否是 hist 的前缀。两次压缩之间模型历史
// 只会追加，所以正常情况下总是成立；这里再检查一次以防万一。
func isPrefix(prefix, hist []protocol.Item) bool {
	if len(prefix) > len(hist) {
		return false
	}
	for i, it := range prefix {
		if hist[i].ID != it.ID {
			return false
		}
	}
	return true
}
