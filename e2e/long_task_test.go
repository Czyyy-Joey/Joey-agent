package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLongTaskCompaction 用 8000 token 的小窗口跑 long_task.json：连续读四个
// 大文件，第三个读完后上下文越过 90% 阈值，SSE 里出现压缩条目，之后任务
// 继续完成并复述原始目标。再执行 /compact 确认手动压缩也能走通。
func TestLongTaskCompaction(t *testing.T) {
	bin := buildJoey(t)
	ws, err := filepath.Abs("../testdata/demo/longtask")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnv("JOEY_CONTEXT_WINDOW=8000")
	// 剧本按顺序回放，后台压缩请求会抢走前台的步骤，这里关掉它；轮末压缩
	// 在这个剧本里达不到水位，保持默认。
	cfg := filepath.Join(t.TempDir(), "joey.toml")
	if err := os.WriteFile(cfg, []byte("[model]\nbackground_compact_percent = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	url, _ := launch(t, bin, env, "-config", cfg, "-script", "../testdata/scenarios/long_task.json", "-workspace", ws)
	id := createSession(t, url, ws)
	sc := stream(t, url, id, 0)
	turnID := submit(t, url, id, "通读 a.md 到 d.md 四个模块文档，最后复述我的目标并列出每个模块的主题")
	evs := readAll(t, sc, turnID)

	var (
		compactions []map[string]any
		outputs     []string
		maxContext  float64
		limit       float64
	)
	for _, e := range evs {
		var d map[string]any
		_ = json.Unmarshal(e.Data, &d)
		switch {
		case e.Type == "item.completed" && d["kind"] == "compaction":
			compactions = append(compactions, d)
		case e.Type == "item.completed" && d["kind"] == "tool_call":
			outputs = append(outputs, d["output"].(string))
		case e.Type == "token.usage":
			if n, _ := d["context_tokens"].(float64); n > maxContext {
				maxContext = n
			}
			limit, _ = d["auto_compact_limit"].(float64)
		}
	}
	s := summarize(evs)
	if s.status != "completed" || !strings.Contains(s.answer, "原始目标") {
		t.Fatalf("turn = %s, answer %q", s.status, s.answer)
	}
	if len(compactions) != 1 {
		t.Fatalf("compactions = %+v", compactions)
	}
	c := compactions[0]
	info := c["compaction"].(map[string]any)
	if c["status"] != "completed" || info["reason"] != "mid_turn" || info["replacement"] != nil ||
		info["tokens_after"].(float64) >= info["tokens_before"].(float64) || !strings.Contains(c["text"].(string), "用户目标") {
		t.Errorf("compaction = %+v", c)
	}
	if limit != 7200 || maxContext < limit {
		t.Errorf("limit = %v, max context = %v", limit, maxContext)
	}
	if len(outputs) != 4 || !strings.Contains(outputs[0], "tokens truncated") {
		t.Errorf("tool outputs = %d, first truncated = %v", len(outputs), len(outputs) > 0 && strings.Contains(outputs[0], "tokens truncated"))
	}

	// 快照里保留完整历史，检查点在原位置。
	info2 := getJSON(t, url+"/api/sessions/"+id)
	var kinds []string
	for _, it := range info2["items"].([]any) {
		kinds = append(kinds, it.(map[string]any)["kind"].(string))
	}
	if !strings.Contains(strings.Join(kinds, ","), "tool_output,compaction,agent_message,tool_call") {
		t.Errorf("items = %v", kinds)
	}

	// 手动 /compact：作为独立轮次运行，压缩后模型历史只剩用户消息和摘要。
	turnID = submit(t, url, id, "/compact")
	evs = readAll(t, sc, turnID)
	manual := 0
	for _, e := range evs {
		var d map[string]any
		_ = json.Unmarshal(e.Data, &d)
		if e.Type == "item.completed" && d["kind"] == "compaction" && d["status"] == "completed" {
			if d["compaction"].(map[string]any)["reason"] != "manual" {
				t.Errorf("manual compaction = %+v", d)
			}
			manual++
		}
	}
	if s := summarize(evs); s.status != "completed" || manual != 1 {
		t.Errorf("/compact turn = %s, %d compactions", s.status, manual)
	}
}
