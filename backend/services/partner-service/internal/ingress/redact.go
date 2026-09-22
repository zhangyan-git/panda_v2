package ingress

import "bytes"

// secretFields 是**进了调用日志就要先抹掉值**的字段名。
//
// 今天只有一个：device/pickup 那条路的 `pickupPassword`——顾客在机器上敲、厂商原样转报过来
// 的那个静态验证码，也就是这台设备扣款的凭据。它在别处都被当成凭据对待：咖啡机域的仓储把它
// 排除在审计快照之外（admin.go 给那个字段打了 `json:"-"`，理由写在 proto 里），proto 里也
// 写明了「凭据多存一处就多一套保留期」。而 partner_call_logs 恰恰是「多存的那一处」：它有
// 自己的保留期、后台调用日志页能读、也能被导出，而且运营在后台把取货码换了之后，旧码仍留在
// 已经写下的日志行里。
//
// **按字段名匹配、不按路径**：同一个字段出现在别的路径上（将来某条新接口复用它）时同样不该
// 进日志，而按路径匹配的写法只在「记得同时改这里」的前提下成立。
var secretFields = [][]byte{
	[]byte(`"pickupPassword"`),
}

// redactSecretFields 把报文里那些字段的**值**替换成 `***`，只动值、不动键。
//
// 键留着是有用的：调用日志的用途是回答「对方到底发了什么」，而「他没填这个字段」与
// 「他填了但我们不记」是两句不同的话。
//
// 它跑在**验签之前**（调用日志在读完报文那一刻就落库了），所以报文可能是坏的、也可能不是
// JSON。这个函数因此不做任何假设：找不到字段、值不是字符串、字符串没有收尾，都原样返回，
// 绝不猜测边界——猜错一次就是把凭据的一部分留在日志里，或者把后半份报文吃掉。
func redactSecretFields(body []byte) []byte {
	var out []byte
	offset := 0
	for _, field := range secretFields {
		for {
			start := bytes.Index(body[offset:], field)
			if start < 0 {
				break
			}
			start += offset
			valueStart, valueEnd, ok := stringValueAt(body, start+len(field))
			if !ok {
				offset = start + len(field)
				continue
			}
			if out == nil {
				out = make([]byte, 0, len(body))
			}
			// valueStart 是**开引号之后**那一个位置，所以这里切到 valueStart-1（含开引号）
			// 的左边——占位符自带一对引号，留着原来那个就成了 `""***"`。
			out = append(out, body[offset:valueStart-1]...)
			out = append(out, `"***"`...)
			offset = valueEnd
		}
	}
	if out == nil {
		return body
	}
	return append(out, body[offset:]...)
}

// stringValueAt 从 `key` 之后接着往下找那个 JSON 字符串值，返回它的引号边界。
//
// 只跳过空白与一个冒号：`"pickupPassword"` 出现在别处（比如某个说明性字段的值里）时不该
// 被当成键，跳过冒号这一步就把那种情况挡掉了。
func stringValueAt(body []byte, from int) (valueStart, valueEnd int, ok bool) {
	i := from
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r' || body[i] == ':') {
		i++
	}
	if i >= len(body) || body[i] != '"' {
		// 值不是字符串（数字、null、对象）——不是这个字段该有的形状，原样留着。
		return 0, 0, false
	}
	j := i + 1
	for j < len(body) && body[j] != '"' {
		// 转义字符后面那一个字节无条件跳过：`"a\"b"` 里的 `\"` 不是字符串的结尾。
		if body[j] == '\\' && j+1 < len(body) {
			j += 2
			continue
		}
		j++
	}
	if j >= len(body) {
		// 引号没有收尾，报文是断的。宁可不抹，也不猜一个边界出来。
		return 0, 0, false
	}
	return i + 1, j + 1, true
}
