package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

const overflowMsg = "Your input exceeds the context window of this model."

func overflow() llmtest.Step { return llmtest.Step{Error: overflowMsg, ErrorStatus: 400} }

func echoCall(id, arg string) llmtest.Step {
	return llmtest.Step{Calls: []llmtest.StepCall{{ID: id, Name: "echo", Arguments: []byte(`{"x":"` + arg + `"}`)}}}
}

// isCompactRequest 报告 req 是否是压缩请求：最后一条是压缩提示词且不带工具。
func isCompactRequest(items []protocol.Item, tools int) bool {
	return len(items) > 0 && items[len(items)-1].Text == history.CompactPrompt && tools == 0
}

func compactions(items []protocol.Item) []protocol.Compaction {
	var out []protocol.Compaction
	for _, it := range items {
		if it.Kind == protocol.ItemCompaction {
			out = append(out, *it.Compaction)
		}
	}
	return out
}

// TestMidTurnCompaction：工具输出让上下文越过阈值后，在下一次采样前压缩；
// 压缩后的请求带着摘要和最初的目标，之后的工具调用照常执行。
func TestMidTurnCompaction(t *testing.T) {
	big := strings.Repeat("z", 2000)
	p := llmtest.NewScripted(
		echoCall("c1", big),
		llmtest.Step{Text: "SUMMARY: goal is X, echoed once"},
		echoCall("c2", "small"),
		llmtest.Step{Text: "done"},
	)
	rec := &recorder{}
	env := newEnv(t, p, rec)
	env.AutoCompactLimit = 600
	res := RunTurn(context.Background(), env, nil, "the original goal X")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	reqs := p.Requests()
	if len(reqs) != 4 || !isCompactRequest(reqs[1].Items, len(reqs[1].Tools)) {
		t.Fatalf("requests = %d, second compact = %v", len(reqs), len(reqs) > 1 && isCompactRequest(reqs[1].Items, len(reqs[1].Tools)))
	}
	after := reqs[2].Items
	if len(after) != 2 || after[0].Text != "the original goal X" || !history.IsSummary(after[1].Text) || !strings.Contains(after[1].Text, "SUMMARY: goal is X") {
		t.Errorf("request after compaction = %+v", after)
	}
	if len(reqs[2].Tools) == 0 {
		t.Error("tools missing after compaction")
	}
	cs := compactions(res.Items)
	if len(cs) != 1 || cs[0].Reason != protocol.CompactMidTurn || cs[0].ItemsBefore != 3 || cs[0].ItemsAfter != 2 || cs[0].TokensAfter >= cs[0].TokensBefore {
		t.Errorf("compactions = %+v", cs)
	}
	// 压缩后的第二个工具调用正常执行，模型历史保持配对。
	final := history.Replay(res.Items)
	if got := kinds(final); !slices.Equal(got, []protocol.ItemKind{
		protocol.ItemUserMessage, protocol.ItemUserMessage, protocol.ItemToolCall, protocol.ItemToolOutput, protocol.ItemAgentMessage,
	}) {
		t.Errorf("model history = %v", got)
	}
	if _, added := history.Normalize(final, history.MissingMessage); len(added) != 0 {
		t.Error("history not normalized")
	}
	want := []string{"item.started:compaction:in_progress", "item.delta:compaction", "item.completed:compaction:completed"}
	var got []string
	for _, s := range rec.types() {
		if strings.Contains(s, "compaction") {
			got = append(got, s)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("compaction events = %v", got)
	}
}

// TestPreTurnCompaction：历史加上新输入超过阈值时，在记录输入之前压缩。
func TestPreTurnCompaction(t *testing.T) {
	var hist []protocol.Item
	for i := range 10 {
		hist = append(hist,
			protocol.Item{ID: protocol.NewID("item"), Kind: protocol.ItemUserMessage, Text: "question " + string(rune('a'+i))},
			protocol.Item{ID: protocol.NewID("item"), Kind: protocol.ItemAgentMessage, Text: strings.Repeat("answer ", 50)},
		)
	}
	p := llmtest.NewScripted(llmtest.Step{Text: "summary of ten answers"}, llmtest.Step{Text: "ok"})
	env := newEnv(t, p, &recorder{})
	env.AutoCompactLimit = 500
	res := RunTurn(context.Background(), env, hist, "new question")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	reqs := p.Requests()
	if !isCompactRequest(reqs[0].Items, len(reqs[0].Tools)) || len(reqs[0].Items) != len(hist)+1 {
		t.Fatalf("first request is not the compaction of the old history")
	}
	last := reqs[1].Items
	if !history.IsSummary(last[len(last)-2].Text) || last[len(last)-1].Text != "new question" {
		t.Errorf("request = %v", last)
	}
	if got := kinds(res.Items); !slices.Equal(got, []protocol.ItemKind{protocol.ItemCompaction, protocol.ItemUserMessage, protocol.ItemAgentMessage}) {
		t.Errorf("items = %v", got)
	}
	if cs := compactions(res.Items); cs[0].Reason != protocol.CompactPreTurn {
		t.Errorf("reason = %s", cs[0].Reason)
	}
}

// TestOverflowCompaction：模型报告上下文超限时压缩后重试一次。压缩请求
// 本身也超限时，删掉最旧的条目再试。
func TestOverflowCompaction(t *testing.T) {
	hist := []protocol.Item{
		{ID: "u0", Kind: protocol.ItemUserMessage, Text: "first goal"},
		{ID: "m0", Kind: protocol.ItemAgentMessage, Text: "ack"},
	}
	p := llmtest.NewScripted(
		overflow(),                     // 正常请求超限
		overflow(),                     // 压缩请求超限
		llmtest.Step{Text: "summary"},  // 删掉一条后成功
		llmtest.Step{Text: "answered"}, // 重试
	)
	rec := &recorder{}
	res := RunTurn(context.Background(), newEnv(t, p, rec), hist, "second goal")
	if res.Status != protocol.TurnCompleted || res.FinalMessage != "answered" {
		t.Fatalf("result = %+v", res)
	}
	reqs := p.Requests()
	if n1, n2 := len(reqs[1].Items), len(reqs[2].Items); n2 != n1-1 || !isCompactRequest(reqs[2].Items, 0) {
		t.Errorf("compaction retry sent %d then %d items", n1, n2)
	}
	retry := reqs[3].Items
	if retry[0].Text != "first goal" || retry[1].Text != "second goal" || !history.IsSummary(retry[2].Text) {
		t.Errorf("retry = %+v", retry)
	}
	if cs := compactions(res.Items); len(cs) != 1 || cs[0].Reason != protocol.CompactOverflow {
		t.Errorf("compactions = %+v", cs)
	}

	// 压缩后仍然超限：轮次失败，不会无限重试。
	p = llmtest.NewScripted(overflow(), llmtest.Step{Text: "summary"}, overflow())
	res = RunTurn(context.Background(), newEnv(t, p, &recorder{}), hist, "again")
	if res.Status != protocol.TurnFailed || len(p.Requests()) != 3 {
		t.Errorf("second overflow: %s after %d requests", res.Status, len(p.Requests()))
	}
}

// TestCompactionFailureContinues：自动压缩失败只发出警告，轮次继续。
func TestCompactionFailureContinues(t *testing.T) {
	p := llmtest.NewScripted(echoCall("c1", strings.Repeat("z", 2000)), llmtest.Step{Text: "  "}, llmtest.Step{Text: "done"})
	rec := &recorder{}
	env := newEnv(t, p, rec)
	env.AutoCompactLimit = 600
	res := RunTurn(context.Background(), env, nil, "goal")
	if res.Status != protocol.TurnCompleted || len(compactions(res.Items)) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Contains(rec.types(), "item.completed:compaction:failed") || !slices.Contains(rec.types(), "warning") {
		t.Errorf("events = %v", rec.types())
	}
}

func TestManualCompact(t *testing.T) {
	hist := []protocol.Item{
		{ID: "u0", Kind: protocol.ItemUserMessage, Text: "goal"},
		{ID: "m0", Kind: protocol.ItemAgentMessage, Text: "answer"},
	}
	p := llmtest.NewScripted(llmtest.Step{Text: "the summary"})
	res := Compact(context.Background(), newEnv(t, p, &recorder{}), hist)
	if res.Status != protocol.TurnCompleted || len(res.Items) != 1 || res.Items[0].Compaction.Reason != protocol.CompactManual || res.Items[0].Text != "the summary" {
		t.Fatalf("result = %+v", res)
	}
	if got := history.Replay(append(hist, res.Items...)); len(got) != 2 || got[0].Text != "goal" {
		t.Errorf("replay = %+v", got)
	}

	res = Compact(context.Background(), newEnv(t, llmtest.NewScripted(), &recorder{}), nil)
	if res.Status != protocol.TurnCompleted || len(res.Items) != 0 {
		t.Errorf("empty = %+v", res)
	}
	res = Compact(context.Background(), newEnv(t, llmtest.NewScripted(llmtest.Step{Error: "boom"}), &recorder{}), hist)
	if res.Status != protocol.TurnFailed {
		t.Errorf("failure = %+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res = Compact(ctx, newEnv(t, llmtest.NewScripted(llmtest.Step{Text: "x"}), &recorder{}), hist)
	if res.Status != protocol.TurnInterrupted {
		t.Errorf("cancelled = %+v", res)
	}
}

// TestCompactionRecordFailure：检查点写库失败时不替换历史。
func TestCompactionRecordFailure(t *testing.T) {
	hist := []protocol.Item{{ID: "u0", Kind: protocol.ItemUserMessage, Text: "goal"}}
	env := newEnv(t, llmtest.NewScripted(llmtest.Step{Text: "summary"}), &recorder{})
	env.Record = func(it protocol.Item) error {
		if it.Kind == protocol.ItemCompaction {
			return errors.New("disk full")
		}
		return nil
	}
	res := Compact(context.Background(), env, hist)
	if res.Status != protocol.TurnFailed || len(res.Items) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestToolOutputTruncated(t *testing.T) {
	p := llmtest.NewScripted(echoCall("c1", strings.Repeat("q", 5000)), llmtest.Step{Text: "ok"})
	env := newEnv(t, p, &recorder{})
	env.ToolOutputTokens = 100
	res := RunTurn(context.Background(), env, nil, "go")
	out := res.Items[2].Output.Content
	if !strings.Contains(out, "tokens truncated") || len(out) > 450 || !strings.HasPrefix(out, "echo:") || !strings.HasSuffix(out, `"}`) {
		t.Errorf("output = %d bytes: %.80s…", len(out), out)
	}
}

// TestContextTokens：token.usage 报告上游用量加上之后条目的估算值。
func TestContextTokens(t *testing.T) {
	p := llmtest.NewScripted(
		llmtest.Step{Calls: echoCall("c1", "x").Calls, Usage: &protocol.Usage{InputTokens: 1000, OutputTokens: 20, TotalTokens: 1020}},
		llmtest.Step{Text: "ok"},
	)
	rec := &recorder{}
	env := newEnv(t, p, rec)
	env.AutoCompactLimit = 5000
	res := RunTurn(context.Background(), env, nil, "go")
	var usages []protocol.UsageData
	for _, e := range rec.events {
		if e.Type == protocol.EventTokenUsage {
			usages = append(usages, e.Data.(protocol.UsageData))
		}
	}
	if len(usages) != 2 {
		t.Fatalf("usage events = %+v", usages)
	}
	out := history.EstimateItem(res.Items[2])
	if u := usages[0]; u.ContextTokens != 1020 || u.Estimated || u.AutoCompactLimit != 5000 {
		t.Errorf("after sampling = %+v", u)
	}
	if u := usages[1]; u.ContextTokens != 1020+out || u.Estimated {
		t.Errorf("after tools = %+v (output %d)", u, out)
	}
	if res.Context.Reported != 1020 || res.Context.Covered != 2 {
		t.Errorf("context = %+v", res.Context)
	}
}

// TestCompactionUsageReported：压缩请求的用量通过 token.usage 的 last 发出，
// 所以按 last 累计的会话用量等于轮次用量。
func TestCompactionUsageReported(t *testing.T) {
	u := func(n int64) *protocol.Usage { return &protocol.Usage{InputTokens: n, TotalTokens: n} }
	p := llmtest.NewScripted(
		llmtest.Step{Calls: echoCall("c1", strings.Repeat("z", 2000)).Calls, Usage: u(100)},
		llmtest.Step{Text: "summary", Usage: u(50)},
		llmtest.Step{Text: "done", Usage: u(20)},
	)
	rec := &recorder{}
	env := newEnv(t, p, rec)
	env.AutoCompactLimit = 300
	res := RunTurn(context.Background(), env, nil, "goal")
	var sum int64
	for _, e := range rec.events {
		if e.Type == protocol.EventTokenUsage {
			sum += e.Data.(protocol.UsageData).Last.TotalTokens
		}
	}
	if res.Usage.TotalTokens != 170 || sum != 170 {
		t.Errorf("turn usage %d, sum of last %d; status %s final %q types %v", res.Usage.TotalTokens, sum, res.Status, res.FinalMessage, rec.types())
	}
}

// TestPostTurnCompaction：模型给出最终回答后，上下文达到轮末水位就在轮次
// 结束前压缩；没达到就不压缩。回答本身不受影响。
func TestPostTurnCompaction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		percent int
		want    bool
	}{{"above", 50, true}, {"below", 90, false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.NewScripted(echoCall("c1", strings.Repeat("a", 400)), llmtest.Step{Text: "answer"}, llmtest.Step{Text: "SUM"})
			env := newEnv(t, p, &recorder{})
			env.AutoCompactLimit = 1000
			env.PostTurnCompactPercent = tc.percent
			res := RunTurn(context.Background(), env, nil, "goal")
			if res.Status != protocol.TurnCompleted || res.FinalMessage != "answer" {
				t.Fatalf("result = %+v", res)
			}
			cs := compactions(res.Items)
			if !tc.want {
				if len(cs) != 0 || len(p.Requests()) != 2 {
					t.Errorf("compacted below the threshold: %+v", cs)
				}
				return
			}
			reqs := p.Requests()
			if len(reqs) != 3 || !isCompactRequest(reqs[2].Items, len(reqs[2].Tools)) {
				t.Fatalf("third request is not a compaction")
			}
			if len(cs) != 1 || cs[0].Reason != protocol.CompactPostTurn || cs[0].Background {
				t.Errorf("compactions = %+v", cs)
			}
			if last := res.Items[len(res.Items)-1]; last.Kind != protocol.ItemCompaction {
				t.Errorf("last item = %s, want the checkpoint after the answer", last.Kind)
			}
			if got := history.Replay(res.Items); len(got) != 2 || got[0].Text != "goal" || !strings.Contains(got[1].Text, "SUM") {
				t.Errorf("next turn starts from %+v", got)
			}
		})
	}
}

// TestBackgroundCompactionAdopted：越过后台水位后在后台生成摘要；之后越过
// 自动压缩阈值时采用它，前台不再发摘要请求。快照之后新增的条目接在摘要
// 后面。
func TestBackgroundCompactionAdopted(t *testing.T) {
	p := llmtest.NewScripted(
		echoCall("c1", strings.Repeat("a", 400)),
		echoCall("c2", strings.Repeat("b", 700)),
		llmtest.Step{Text: "done"},
	)
	bgp := llmtest.NewScripted(llmtest.Step{Text: "BG SUMMARY"})
	b := NewBackground(context.Background(), bgp)
	defer b.Close()
	env := newEnv(t, p, &recorder{})
	env.AutoCompactLimit = 1000
	env.BackgroundCompactPercent = 50
	env.Background = b
	res := RunTurn(context.Background(), env, nil, "goal")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	for i, r := range p.Requests() {
		if isCompactRequest(r.Items, len(r.Tools)) {
			t.Errorf("foreground request %d is a compaction", i)
		}
	}
	bg := bgp.Requests()
	if len(bg) != 1 || !isCompactRequest(bg[0].Items, len(bg[0].Tools)) || len(bg[0].Items) != 4 {
		t.Fatalf("background requests = %+v", bg)
	}
	cs := compactions(res.Items)
	if len(cs) != 1 || cs[0].Reason != protocol.CompactMidTurn || !cs[0].Background {
		t.Fatalf("compactions = %+v", cs)
	}
	after := p.Requests()[2].Items
	if got := kinds(after); !slices.Equal(got, []protocol.ItemKind{
		protocol.ItemUserMessage, protocol.ItemUserMessage, protocol.ItemToolCall, protocol.ItemToolOutput,
	}) || !strings.Contains(after[1].Text, "BG SUMMARY") || after[2].Call.CallID != "c2" {
		t.Errorf("request after adoption = %v", got)
	}
}

// TestBackgroundAdoptedAtNextTurnEnd：轮末启动的后台摘要在下一轮结束时
// 完成并被采用（没达到轮末压缩水位也采用）。
func TestBackgroundAdoptedAtNextTurnEnd(t *testing.T) {
	bgp := llmtest.NewScripted(llmtest.Step{Text: "BG", Delay: llmtest.Duration(300 * time.Millisecond)})
	b := NewBackground(context.Background(), bgp)
	defer b.Close()
	newTurnEnv := func(p *llmtest.Scripted) TurnEnv {
		env := newEnv(t, p, &recorder{})
		env.AutoCompactLimit = 1000
		env.PostTurnCompactPercent = 90
		env.BackgroundCompactPercent = 50
		env.Background = b
		return env
	}
	p1 := llmtest.NewScripted(echoCall("c1", strings.Repeat("a", 400)), llmtest.Step{Text: "answer"})
	res1 := RunTurn(context.Background(), newTurnEnv(p1), nil, "goal")
	if res1.Status != protocol.TurnCompleted || len(compactions(res1.Items)) != 0 {
		t.Fatalf("turn 1 = %+v", res1)
	}
	b.mu.Lock()
	c := b.pending
	b.mu.Unlock()
	if c == nil {
		t.Fatal("no background compaction was started")
	}
	<-c.done

	p2 := llmtest.NewScripted(llmtest.Step{Text: "answer 2"})
	res2 := RunTurn(context.Background(), newTurnEnv(p2), res1.Items, "next")
	cs := compactions(res2.Items)
	if res2.Status != protocol.TurnCompleted || len(cs) != 1 || cs[0].Reason != protocol.CompactPostTurn || !cs[0].Background {
		t.Fatalf("turn 2 compactions = %+v", cs)
	}
	if len(p2.Requests()) != 1 || len(bgp.Requests()) != 1 {
		t.Errorf("requests: foreground %d, background %d", len(p2.Requests()), len(bgp.Requests()))
	}
	// 快照是在第一轮执行完工具时拍的，所以第一轮的回答和第二轮都接在摘要后面。
	got := history.Replay(append(slices.Clone(res1.Items), res2.Items...))
	var texts []string
	for _, it := range got {
		texts = append(texts, it.Text)
	}
	if len(got) != 5 || texts[0] != "goal" || !strings.HasSuffix(texts[1], "\nBG") || texts[2] != "answer" || texts[3] != "next" || texts[4] != "answer 2" {
		t.Errorf("history after adoption = %q", texts)
	}
}

func TestBackgroundLifecycle(t *testing.T) {
	hist := []protocol.Item{{ID: "u", Kind: protocol.ItemUserMessage, Text: "goal"}}
	wait := func(b *Background) *candidate {
		b.mu.Lock()
		c := b.pending
		b.mu.Unlock()
		if c != nil {
			<-c.done
		}
		return c
	}

	// 每一代只尝试一次。
	p := llmtest.NewScripted(llmtest.Step{Text: "s1"}, llmtest.Step{Text: "s2"})
	b := NewBackground(context.Background(), p)
	b.start("sys", hist)
	b.start("sys", hist)
	wait(b)
	if n := len(p.Requests()); n != 1 {
		t.Errorf("requests = %d", n)
	}
	// 历史被替换（新的一代）后旧结果作废，可以再试一次。
	b.advance()
	if c := b.take(context.Background(), hist, true); c != nil {
		t.Error("stale result was adopted")
	}
	b.start("sys", hist)
	wait(b)
	if c := b.take(context.Background(), append(hist, protocol.Item{ID: "x"}), false); c == nil || c.summary != "s2" {
		t.Errorf("take = %+v", c)
	}
	if c := b.take(context.Background(), hist, false); c != nil {
		t.Error("a result was adopted twice")
	}
	b.Close()

	// 快照不再是历史的前缀、或请求失败时不采用。
	b = NewBackground(context.Background(), llmtest.NewScripted(llmtest.Step{Text: "s"}))
	b.start("sys", hist)
	wait(b)
	if c := b.take(context.Background(), []protocol.Item{{ID: "other"}}, false); c != nil {
		t.Error("adopted a summary of a different history")
	}
	b.Close()
	b = NewBackground(context.Background(), llmtest.NewScripted(llmtest.Step{Error: "boom"}))
	b.start("sys", hist)
	wait(b)
	if c := b.take(context.Background(), hist, false); c != nil {
		t.Error("adopted a failed request")
	}
	b.Close()

	// Close 取消进行中的请求并等待它退出。
	b = NewBackground(context.Background(), llmtest.NewScripted(llmtest.Step{Hang: true}))
	b.start("sys", hist)
	done := make(chan struct{})
	go func() { b.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the background request")
	}
	b.start("sys", hist)
	if b.pending != nil {
		t.Error("started after Close")
	}
}
