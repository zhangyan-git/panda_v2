package ums

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 本文件是这一族的**第三套**签名：入站报文的 key 拼接签名。
//
// # 三套签名，别混
//
//   - OPEN-BODY-SIG —— 出站 POST：拼 Authorization 头，签的是报文体（ums.go 的 Sign）；
//   - OPEN-FORM-PARAM —— 出站 GET（H5 下单）：同一套算法，参数进查询串（ums.go 的 signedQuery）；
//   - **本文件** —— 入站（支付结果通知 + 结果页回跳）：参数按名字排序拼串、末尾接通讯密钥，
//     再取 MD5 或 SHA256。
//
// 前两套的待签串同形（`appId + 时间戳 + 随机串 + sha256 hex(报文体)`），第三套与它们**没有
// 一个字相同**：它没有 appId、没有时间戳、没有随机串，也不是 HMAC。把它合并进 Sign 的后果
// 是两条路一起坏掉（一侧永远验不过），而这正是规范自己列出来的坑（文档 §9.5）。
//
// # 它是**入站**的，所以是我们在验
//
// 出站那两套是我们算给渠道验的；这一套反过来——渠道算、我们验。所以这里的每一行都在回答
// 「渠道当时是怎么算的」，而不是「我们希望它怎么算」。凡是文档没写死的地方（见下面 signType
// 与 hex 大小写那两段），判断的依据都是「哪种判法不容易把一条真的收款通知拒掉」。
//
// # 算法已对过厂商向量；没对上的那一格是密钥
//
// 老系统那边**根本没有验签**（ParseNotify 是空壳，见 notify.go 上那段注释），所以这里没有
// 任何可对照的实现，最初是照规范推的。2026-09-21 拿真实账户打了一笔 H5 支付，把这条推的
// 前提对完了：
//
//   - 真实回调**确实**是这一套：表单体、参数 `sign`、`signType=SHA256`、参数集与规范示例同形；
//   - 算法本身逐字对上——规范 §1.9.4 印了待签串 + MD5 + SHA256 三样，§1.10 印了一条完整的
//     通知 URL，两边用这里的两个函数都能算出与规范印的一模一样的摘要（见
//     keyappend_spec_test.go，那两条向量直接取自规范）；
//   - 唯一对不上的是**密钥**：那次实测里服务拿的是启动脚本的兜底值（`PAYMENT_UMS_COMM_KEY`
//     没配），所以每一条回调都被判签名不符。**这不影响任何一行代码**——换上真密钥即可。
//
// 那次"验不过"的现场还留下一个值得记的判据：回调被拒之后，钱**没有**卡住——主动查单那条路
// （`internal/worker`）在 4 分钟后把这笔查成了 TRADE_SUCCESS，支付单照样结掉、订单照样变
// paid。所以这条路是 fail closed 且**有兜底**的：拒收一条来路不明的通知不会丢钱，只是慢一点。

// 这一族的入站报文里参数的**名字**，只列本文件用到的几个。
//
// merOrderId 与 status 逐字与出站报文（createBody / h5CreateBody 的 json tag）相同：渠道把
// 商户订单号原样带回来，回调靠它找到我们的支付单。
const (
	paramMerOrderID = "merOrderId"
	paramSignType   = "signType"
)

// signType 的取值。它们进待签串吗？**不进**——signType 本身是一个普通参数，照常参与排序拼串，
// 这里这两个常量只是我们用来选摘要算法的。
const (
	signTypeMD5    = "MD5"
	signTypeSHA256 = "SHA256"
)

// signatureParams 是「这个参数装的是签名本身」的候选名，验签时必须从待签串里摘掉。
//
// 规范对结果页明说是 `sign`（原文 §1.9.3「其中 sign 是必返项」），对支付结果通知一个名字都
// 没给。两个都摘，理由是**签名盖不住它自己**：若把签名参数也算进待签串，那个待签串里就含着
// 由它自己算出来的摘要，任何人都构造不出来。
//
// 摘错一个业务参数的风险是「这个渠道永远验不过签」；不摘的风险是「一条都验不过」。两者都是
// 响亮的失败，而业务参数恰好叫 sign 或 signature 的可能性低到可以忽略。
var signatureParams = []string{"signature", "sign"}

// isSignatureParam 判断一个参数名是不是「装着签名本身」的那一个。
//
// 用 EqualFold：参数名理论上大小写敏感，但这里的判断只影响「要不要把它从待签串里摘掉」，
// 而渠道把 `Sign` 当成一个业务参数签进串里的可能性，远小于它把签名参数写成 `Sign` 的可能性。
func isSignatureParam(name string) bool {
	for _, candidate := range signatureParams {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

// givenSignature 从报文里取出渠道给的那个签名值。
//
// 取不到返回空串，调用方据此拒绝——**空串不是一个合法的签名**，它连摘要的长度都不够。
func givenSignature(params map[string]string) string {
	for _, candidate := range signatureParams {
		for name, value := range params {
			if strings.EqualFold(name, candidate) && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

// resolveSignType 把报文里的 signType 归一成我们认的两个取值之一。
//
// **缺省是 SHA256**（规范原文 §1.9.3 第 4 条：默认 SHA256，实际用哪种看报文里的 signType）。
// 空串按缺省处理而不是报错：一条不带 signType 的回调是完全可能的，而拒绝它的代价是收不到钱。
//
// 认 `SHA-256` 这种带横杠的写法，靠的是先把横杠去掉：这个值的拼法在渠道侧各处的实现里不统一，
// 而认不出的代价同样是「一条回调都收不下」。**认不出时不静默回落**——回一句明确的错，比拿
// SHA256 去验一份 RSA 签名（然后报「签名错误」）有用得多。
func resolveSignType(raw string) (string, error) {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), "-", "")) {
	case "":
		return signTypeSHA256, nil
	case signTypeMD5:
		return signTypeMD5, nil
	case signTypeSHA256:
		return signTypeSHA256, nil
	}
	return "", fmt.Errorf("unsupported %s %q (this family knows %s and %s)",
		paramSignType, raw, signTypeMD5, signTypeSHA256)
}

// keyAppendPayload 拼出那一串待签原文。
//
// 规则逐条照规范（原文 §1.9.3）：
//
//   - 参数（含名字与值）按名字的 **ASCII 字典序**排序——sort.Strings 对 ASCII 就是字节序；
//   - 用 `&` 连成 `k=v&k=v`；
//   - **末尾直接接上通讯密钥**：不是 HMAC，不是 base64，就是拼接，连分隔符都没有；
//   - **无值（含空串）的参数不参与**；签名参数自己也不参与（见 signatureParams）。
//
// # 值是**解码后**的
//
// 规范说「值里有特殊字符要 URLEncode，但签名用原始值」。在入站这一侧，这两句话合起来的意思
// 是：渠道把原始值编码后放进报文，**我们解回来再签**。url.ParseQuery 已经做了那一步，所以
// 这里拿到的是原始值本身——不要在这里再 Decode 一次，也不要反过来拿 RawQuery 去签。
//
// 名字为空（`=v` 这种畸形参数）的一并跳过：它排出来是 `=v`，那不是任何一个渠道会签的东西。
func keyAppendPayload(params map[string]string, secret string) string {
	names := make([]string, 0, len(params))
	for name, value := range params {
		if name == "" || value == "" || isSignatureParam(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var builder strings.Builder
	for _, name := range names {
		if builder.Len() > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(params[name])
	}
	builder.WriteString(secret)
	return builder.String()
}

// keyAppendSignature 按 signType 取 MD5 或 SHA256，回 **小写 hex**。
//
// 不导出（对比 Payload / Sign 那两份是导出的）：入站在这个仓库里没有「试跑」这样的调用方，
// 而它唯一的用处是把通讯密钥拼进结果串——一个返回「含密钥的字符串」的导出函数，迟早会有人
// 把它打进日志。
func keyAppendSignature(params map[string]string, secret, signType string) string {
	payload := keyAppendPayload(params, secret)
	if signType == signTypeMD5 {
		sum := md5.Sum([]byte(payload))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// signatureMatches 比一次签名。
//
// # 为什么折成小写再比
//
// hex 摘要的大小写**不是信息**：同一个摘要写成 `a3f1` 还是 `A3F1` 说的是同一件事。渠道用它
// 内部实现决定的那种写法（`fmt.Sprintf("%x")` 是小写、`%X` 与 Java 的 `toUpperCase()` 是大写，
// 规范里两种都没写），而这一点我们**没有任何文档可以定稿**。判错的代价是这一族一条回调都收
// 不下——正是文档 §8.2 第 7 条描述的那类失败：日志里全是我们自己的错，看起来却像渠道在乱发。
//
// 折大小写不削弱任何东西：对手要伪造的是一份**用密钥算出来的摘要**，而把大小写归一对他没有
// 任何帮助。比较本身走 hmac.Equal（常量时间），与出站那条路同一个函数。
func signatureMatches(given, expected string) bool {
	return hmac.Equal([]byte(strings.ToLower(strings.TrimSpace(given))), []byte(strings.ToLower(expected)))
}

// inboundParams 从一次入站请求里取出「参数名 → 值」。
//
// 两个来源，按**方法**分：GET 的入站请求（银联 H5 的结果页回跳）参数全在查询串里，POST 的
// （支付结果通知）参数全在体里。不合并两者：合并会让「签名盖的是哪些字符」变得说不清，而
// 一份签名盖的是什么，是这条路唯一的判据。
func inboundParams(req provider.NotificationRequest) (map[string]string, error) {
	if req.HTTPMethod == http.MethodGet {
		return flattenValues(req.Query), nil
	}
	return bodyParams(req.Body)
}

// bodyParams 按**内容**把报文体解成参数表：以 `{` 开头就当 JSON 对象，否则当表单。
//
// 按内容而不是 Content-Type，照 formmd5 那条先例：渠道把 JSON 标成 text/plain、把表单标成
// application/json 都是常事，而按头部认的后果是一整段 JSON 被当成一张只有一个怪键的表单，
// 接着验签失败——那时看起来像是签名算法错了，真正错的是我们读报文的方式。
//
// **两种都要认**：规范对支付结果通知写的是 form 表单，而老系统的回调入口按 Content-Type
// 分流成 JSON 或表单两种 map（order_handler.go:1043），说明真实投递里两种都出现过。只认
// 表单的写法会在渠道改用 JSON 的那一天把全部回调拒掉。
func bodyParams(body []byte) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]any
		if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
			return nil, fmt.Errorf("body starts with { but is not a json object: %w", err)
		}
		params := make(map[string]string, len(object))
		for name, value := range object {
			params[name] = scalarText(value)
		}
		return params, nil
	}
	values, err := url.ParseQuery(trimmed)
	if err != nil {
		return nil, fmt.Errorf("body is neither a json object nor a form: %w", err)
	}
	return flattenValues(values), nil
}

// flattenValues 把 url.Values 压成「一个名字一个值」。
//
// **同名参数只取第一个**：这一族的报文是扁平的，一个名字出现两次不在协议里。取第一个而不是
// 报错，是因为「报错」在这条路上的后果是拒收——而一份我们今天读不懂的报文，未必是伪造的。
// 真出现同名参数时，待签串会与渠道签的东西对不上，那时仍然是一次响亮的失败。
func flattenValues(values url.Values) map[string]string {
	params := make(map[string]string, len(values))
	for name, list := range values {
		if len(list) > 0 {
			params[name] = list[0]
		}
	}
	return params
}

// scalarText 把一个 JSON 值渲染成它进待签串的那个字符串。
//
// 浮点走 strconv 的 'f' 而不是 `%v`：`%v` 对浮点用的是 `%g`，12800 会渲染成 `1.28e+04`，
// 而那个串进待签串以后与渠道签的东西对不上——验签失败，但报文看上去完全正常。
//
// 嵌套的对象与数组不在这一族的报文里（参数全是扁平的）。真出现时用 Go 的默认格式渲染：
// 那个串几乎一定与渠道签的不一样，也就是说验签会**响亮地失败**，而不是把一份我们根本没读懂
// 的报文当合法收下。
func scalarText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// paramsAsJSON 把参数表拼回一份 JSON 对象，交给**既有**的报文解码路径。
//
// 这样做的收益是「回调的字段与处置只有一份」：paymentCallback 的结构、成功状态的三个写法、
// 「成功必须有金额」那一条，都只写在 notify.go 里一处，表单投递与 JSON 投递走的是同一段
// 归一化。代价是数字变成了字符串——而 amountFen 本来就同时吃两种形状（老系统的回调入口按
// Content-Type 分流，表单那条路上每个值都是字符串，所以那件事是这条渠道的既有事实）。
//
// map 的遍历顺序是随机的，但 json.Marshal 会按键排序，所以同样的参数表每次都拼出同样的字节。
func paramsAsJSON(params map[string]string) ([]byte, error) {
	object := make(map[string]any, len(params))
	for name, value := range params {
		object[name] = value
	}
	return json.Marshal(object)
}

// inboundDigest 是验签失败时能安全记下来的那份摘要。
//
// 它**只有参数名、一个布尔与 signType 的取值**，一个参数值都不含——回调报文里可能有用户的
// 手机号与卡号后四位，而这段摘要会被写进 payment_notifications.failure_message 与日志。
//
// 它存在的理由是一条**待确认项**：渠道的回调到底带不带签名、带的是哪一套，既没有文档也没有
// 老系统实现可以对照（见 ums.go 包注释与文档 §10.1）。把参数名记下来，第一条真实回调就能
// 自己说明它是什么形状，不必去渠道那边要一份抓包。
//
// 参数名截断到 failureMessageLimit：一份畸形的、带几千个参数名的报文体也能进这里，而它没有
// 理由把整列日志撑爆。
func inboundDigest(params map[string]string, hasAuthorization bool) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("params=[%s] authorization=%t signType=%q",
		truncate(strings.Join(names, " "), failureMessageLimit), hasAuthorization, params[paramSignType])
}
