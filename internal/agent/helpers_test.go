package agent

import (
	"context"
	"iter"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
)

// lengthProvider 将每个 completed 事件的 finish reason 改写为 "length"。
type lengthProvider struct{ inner llm.Provider }

func (p lengthProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for ev, err := range p.inner.Stream(ctx, req) {
			if ev.Kind == llm.EventCompleted {
				ev.FinishReason = "length"
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}
