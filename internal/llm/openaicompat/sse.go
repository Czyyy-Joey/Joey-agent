package openaicompat

import (
	"bufio"
	"bytes"
	"io"
	"iter"
)

// maxSSELine 限制单条 SSE 行的长度；工具参数可能很大。
const maxSSELine = 8 << 20

// sseData 产生 r 中每个 SSE 事件的 data 载荷。
// 根据 SSE 规范，一个事件中的多行 data 会用 "\n" 连接。
// 注释行和其他字段（event、id、retry）会被忽略。
func sseData(r io.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
		var data []byte
		has := false
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				if has {
					if !yield(data, nil) {
						return
					}
					data, has = nil, false
				}
				continue
			}
			if line[0] == ':' {
				continue
			}
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) != "data" {
				continue
			}
			value = bytes.TrimPrefix(value, []byte(" "))
			if has {
				data = append(data, '\n')
			}
			data = append(data, value...)
			has = true
		}
		if err := sc.Err(); err != nil {
			yield(nil, err)
			return
		}
		if has {
			yield(data, nil)
		}
	}
}
