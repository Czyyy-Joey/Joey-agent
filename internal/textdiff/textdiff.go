// Package textdiff 生成按行比较的 unified diff，用于在界面上展示文件改动。
package textdiff

import (
	"fmt"
	"strings"
)

const (
	// contextLines 是每个改动块前后保留的不变行数，与 git diff 默认值相同。
	contextLines = 3
	// maxEditDistance 限制 Myers 算法搜索的编辑距离。超过时退化为"整段删除、
	// 整段新增"，避免两个差别很大的大文件占用大量内存。
	maxEditDistance = 2000
	// MaxBytes 是 Unified 输出的上限，超出部分截断。
	MaxBytes = 256 << 10
)

// Unified 返回从 old 到 new 的 unified diff。oldName、newName 是展示用的
// 文件名，"/dev/null" 表示文件不存在（新建或删除）。内容相同时，名字也
// 相同则返回 ""，名字不同（改名）则只返回文件头。
func Unified(oldName, newName, old, new string) string {
	if old == new && oldName == newName {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- " + label("a/", oldName) + "\n")
	b.WriteString("+++ " + label("b/", newName) + "\n")
	if old == new {
		return b.String()
	}
	if strings.IndexByte(old, 0) >= 0 || strings.IndexByte(new, 0) >= 0 {
		b.WriteString("Binary files differ\n")
		return b.String()
	}
	ops := diffLines(splitLines(old), splitLines(new))
	for _, h := range hunks(ops) {
		if !writeHunk(&b, ops[h[0]:h[1]]) {
			b.WriteString("... (diff truncated)\n")
			break
		}
	}
	return b.String()
}

func label(prefix, name string) string {
	if name == "/dev/null" {
		return name
	}
	return prefix + name
}

// splitLines 按行切分，每行保留结尾的换行符，这样"末尾有没有换行"也会
// 参与比较。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type op struct {
	kind byte // ' '、'-'、'+'
	line string
	// ai、bi 是这一行之前 old、new 已经消耗的行数，用于计算块头的行号。
	ai, bi int
}

// diffLines 先去掉相同的开头和结尾，再对中间部分运行 Myers 算法。
func diffLines(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]
	mid, ok := myers(midA, midB)
	if !ok {
		mid = nil
		for _, l := range midA {
			mid = append(mid, op{kind: '-', line: l})
		}
		for _, l := range midB {
			mid = append(mid, op{kind: '+', line: l})
		}
	}

	ops := make([]op, 0, pre+len(mid)+suf)
	for _, l := range a[:pre] {
		ops = append(ops, op{kind: ' ', line: l})
	}
	ops = append(ops, mid...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, op{kind: ' ', line: l})
	}
	ai, bi := 0, 0
	for i := range ops {
		ops[i].ai, ops[i].bi = ai, bi
		if ops[i].kind != '+' {
			ai++
		}
		if ops[i].kind != '-' {
			bi++
		}
	}
	return ops
}

// myers 返回把 a 变成 b 的最短编辑序列（Myers 1986 的 O(ND) 算法）。编辑
// 距离超过 maxEditDistance 时返回 false。
func myers(a, b []string) ([]op, bool) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, true
	}
	limit := min(n+m, maxEditDistance)
	off := limit + 1
	v := make([]int, 2*limit+3)
	// trace[d] 是第 d 步结束时对角线 -d..d 上走到的最远 x，下标为 k+d。
	var trace [][]int
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, d), true
			}
		}
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
	}
	return nil, false
}

func backtrack(a, b []string, trace [][]int, depth int) []op {
	x, y := len(a), len(b)
	var rev []op
	for d := depth; d > 0; d-- {
		prev := trace[d-1]
		at := func(k int) int { return prev[k+d-1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, op{kind: ' ', line: a[x]})
		}
		if prevK == k+1 {
			y--
			rev = append(rev, op{kind: '+', line: b[y]})
		} else {
			x--
			rev = append(rev, op{kind: '-', line: a[x]})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, op{kind: ' ', line: a[x]})
	}
	out := make([]op, len(rev))
	for i, o := range rev {
		out[len(rev)-1-i] = o
	}
	return out
}

// hunks 把编辑序列分成若干块，返回每块在 ops 中的 [起, 止) 下标。两处改动
// 之间的不变行不超过 2*contextLines 时合并成一块。
func hunks(ops []op) [][2]int {
	var out [][2]int
	last := 0
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-contextLines, last)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			j := end
			for j < len(ops) && ops[j].kind == ' ' {
				j++
			}
			if j == len(ops) || j-end > 2*contextLines {
				end = min(end+contextLines, j)
				break
			}
			end = j
		}
		out = append(out, [2]int{start, end})
		last, i = end, end
	}
	return out
}

// writeHunk 写出一个块；加上它会超过 MaxBytes 时不写并返回 false。
func writeHunk(b *strings.Builder, ops []op) bool {
	oldCount, newCount := 0, 0
	for _, o := range ops {
		if o.kind != '+' {
			oldCount++
		}
		if o.kind != '-' {
			newCount++
		}
	}
	var h strings.Builder
	fmt.Fprintf(&h, "@@ -%s +%s @@\n", rangeOf(ops[0].ai, oldCount), rangeOf(ops[0].bi, newCount))
	for _, o := range ops {
		h.WriteByte(o.kind)
		if strings.HasSuffix(o.line, "\n") {
			h.WriteString(o.line)
		} else {
			h.WriteString(o.line + "\n\\ No newline at end of file\n")
		}
	}
	if b.Len()+h.Len() > MaxBytes {
		return false
	}
	b.WriteString(h.String())
	return true
}

// rangeOf 按 unified diff 的约定写出起始行和行数：行数为 0 时起始行是
// 改动位置之前的那一行。
func rangeOf(before, count int) string {
	start := before + 1
	if count == 0 {
		start = before
	}
	if count == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
