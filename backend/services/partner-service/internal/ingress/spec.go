package ingress

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/signing"
)

// 认证头。名字与老系统逐字一致（方案 1.1）——合作方那边已经实现的客户端不用改，
// 而 X-Nonce 是他们唯一要新加的一行。
const (
	HeaderAPIKey    = "X-API-Key"
	HeaderTimestamp = "X-Timestamp"
	HeaderNonce     = "X-Nonce"
	HeaderSignature = "X-Signature"
)

// Window 是 X-Timestamp 允许的偏移，也是 nonce 的去重 TTL。
//
// 两者共用一个值不是巧合：nonce 要挡的正是「窗口之内重放同一次请求」，所以它的存活时间必须
// **至少**等于窗口。短了会留下一个「时间戳还有效、nonce 已过期」的空档，长了只是多占一点
// Redis 内存。取相等是这两件事唯一的自洽取值。
//
// 老系统是 300 秒（merchant_api_service.go 的 ValidateTimestamp），保持一致：窗口变了，
// 合作方那边的时钟同步要求也跟着变，这是一个对外承诺。
const Window = 5 * time.Minute

// maxBodyBytes 是我们愿意为一个 JSON 报文读进内存的上限。
//
// 开放接口这一批以查询与发券为主，加上一条设备回执（它也只带六行字段），1 MiB 足够；
// 而没有上限的读会让一个合作方（或一个拿了他
// 密钥的人）用一次 POST 把本服务的内存吃掉。超限回 413，不是 401——报文太大与凭据不对
// 是两件事，混成一句话会让对接的人去查签名。
const maxBodyBytes = 1 << 20

// spec 是 V2 入向唯一的签名规则。
//
//   - SkipEmpty：方案 1.2 第 2 步的「过滤空值」。老系统的对外 API 那版**不过滤**
//     （merchant_api_service.go 的 GenerateSignature 直接遍历全部键），所以一份带空值参数的
//     请求在老系统与 V2 上会签出不同的串。这是有意的收紧：空值参数在待签串里只贡献
//     "k="，它对「报文有没有被改」这个判断没有任何帮助，却让「`?a=` 与 `?a`」这两种写法
//     有了可观察的差别。
//   - Sort：Go 的 sort.Strings 是逐字节升序，与老系统的 sort.Strings 是同一份实现，
//     所以「ASCII 升序」这一条两边逐字节一致。
//   - KeyName "secret"：与老系统对外 API 那版的 `"&secret=" + secret` 逐字一致。
//   - Omit 为空：签名值走 X-Signature 头，不在参数集里，所以不存在老系统那个
//     「sign 字段自指」的问题（那边要 `k != "sign"` 把它排除掉）。
var spec = signing.Spec{
	Canonical: signing.Canonical{SkipEmpty: true, Sort: true, KeyName: "secret"},
	Algorithm: signing.HMACSHA256Hex,
}

// Spec 暴露这份规则给测试与对接文档用（只读）。
func Spec() signing.Spec { return spec }

// Params 拼出待签参数集。**这是本包唯一一处定义「签什么」的地方**，验签与测试都走它。
//
// 键名与取值规则逐条对着老系统的 buildSignatureParams（merchant_api_handler.go:622），
// 只多一个 nonce：
//
//	timestamp  X-Timestamp 原样（字符串，不重新格式化：合作方签的是它发出来的那一串）
//	path       r.URL.Path（规范化后，见包注释）
//	method     r.Method
//	nonce      X-Nonce 原样      ← V2 多出来的
//	query      URL.Query()，每个键取**第一个**值
//	body       JSON 顶层，每个键一个参数
//
// 与老系统那一份的差别**不止 nonce 一个**，四条都写在 spec_test.go 的对照测试里（每一条各有
// 一个测试盯着），其中两处是 V2 有意的收紧：
//
//   - 空值参数不进串（SkipEmpty，老系统遍历全部键）；
//   - **body 不看方法**：老系统那一句是 `if Method == "POST"`，于是 GET / PUT / PATCH /
//     DELETE 的 body 完全在签名之外，改掉它签名照样对得上。V2 对每个方法都打平——方案 §1.2
//     的参数清单里也没有按方法限定。这棵树上今天已经有了一条 POST（设备回执，见
//     routes/openapi.go），所以这条差异**已经能被观察到**：一个照抄老系统示例、把 body 打平
//     限定在 POST 上的客户端，在 PUT/PATCH 上会验签失败——那时先来这里看，不是签名算法有问题。
//
// # 为什么 nonce 必须在待签串里
//
// 方案 1.2 的参数清单里没有它。它必须在。反证很短：nonce 不进串时，攻击者抓到一个请求后
// 把它放到窗口里的任意时刻重发，只需要把 X-Nonce 换成一个没用过的值——签名覆盖的是
// timestamp/path/method/query/body，这些一个字节都没改，验签通过；nonce 是新值，SETNX 也
// 通过。于是那次去重检查只是在「攻击者懒得改 nonce」时才有用。进了串之后，改 nonce 与改
// body 是同一类操作：都必须重新签，而重新签需要密钥。
//
// # body 的两种形状
//
// 打平（老系统的做法）与「对原始字节做摘要」（方案 1.4 第 3 条的措辞）挡的是同一件事——
// 报文被改。选打平是因为**对接方已经实现了它**：老系统的对外 API 就是这么签的，合作方
// 手上的示例代码与文档不必重写。打平还有一个副作用是好的：它有嵌套也认（非字符串值走
// json.Marshal 进串），所以「中间人把 {"a":1} 改成 {"a":2}」与「把 {"a":{"b":1}} 改成
// {"a":{"b":2}}」都会被发现。
//
// 代价写在明处：打平是**规范化**的，所以 byte 级的改写（重排键、改缩进）不会导致验签失败。
// 对这批接口（查询、发券）这不构成风险——报文里没有一个字段是「必须逐字节原样」的。
func Params(r *http.Request, body []byte) map[string]string {
	params := map[string]string{
		"timestamp": r.Header.Get(HeaderTimestamp),
		"path":      signedPath(r.URL),
		"method":    r.Method,
		"nonce":     r.Header.Get(HeaderNonce),
	}
	if r.URL != nil {
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				params[key] = values[0]
			}
		}
	}
	// body 的键覆盖 query 的同名键——与老系统同一个顺序（它也是先 query 后 body）。
	// 覆盖而不是拒绝重名：拒绝会把「query 里带一个同名参数」变成一次 400，而那种请求
	// 在老系统上是能通的。
	for key, value := range FlattenBody(body) {
		params[key] = value
	}
	return params
}

// FlattenBody 把 JSON 顶层打平成 k=v。空 body 或解不出来的 body 返回空 map。
//
// 解不出来返回空 map 而不是错误：JSON 合不合法由 controller 那边的 decodeJSON 判（它会回
// 一句 400），验签这一步不该替它做决定。代价是「一个非法 JSON 的请求」的待签串里只有头与
// query——那仍然是可签的，所以它不会在验签上「意外通过」，只会照常被后面的解析拒掉。
//
// 值的规则与老系统逐条相同：字符串原样，其余走 json.Marshal。数组与嵌套对象因此进串的是
// 它们的紧凑 JSON 形式。
func FlattenBody(body []byte) map[string]string {
	if len(body) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	flat := make(map[string]string, len(decoded))
	for key, value := range decoded {
		if text, ok := value.(string); ok {
			flat[key] = text
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			// 单值序列化失败（JSON 里不该出现）时跳过这一个键，而不是让整份请求失败：
			// 跳过的后果是「这个键不在待签串里」，而调用方签的时候也跳过了同一个键
			// ——它那边用的是同一份 json.Marshal。
			continue
		}
		flat[key] = string(encoded)
	}
	return flat
}

// Canonical 返回**待签串本身**。只给测试与对接文档用：线上路径走 Verify。
//
// 单独暴露它，是因为合作方最常问的一句话是「你们到底把哪个串丢进 HMAC」——有这个函数，
// 排查时可以直接把串打出来对，不必照着文档猜。
func Canonical(params map[string]string, secret string) string {
	return spec.Canonical.Build(params, secret)
}

// Verify 用 platform/signing 校验一个签名。
//
// 它**不判**时间窗与 nonce——那两件事分别属于「时钟」与「状态」，与「这份参数集与这个签名
// 对不对得上」是三个独立的问题，混在一个函数里就没法单独解释是哪一个失败了（而 error_code
// 要的正是这个区分）。
//
// 恒时比较在 signing.VerifyPayload 里（hmac.Equal）。老系统的三处验签都是 `!=`，本实现
// 不继承那条时序侧信道。
func Verify(params map[string]string, secret, provided string) error {
	if strings.TrimSpace(secret) == "" {
		// 与 signing.ErrKeyNotConfigured 分开说：能走到这里的密钥来自库里的密文信封，
		// 而它是空的只可能是**数据坏了**（解出来是空串），不是「没配」。放行是绝对不行的
		// ——那等于任何签名都通过。
		return fmt.Errorf("%w: partner signing secret is empty", signing.ErrKeyNotConfigured)
	}
	return signing.Verify(params, spec, signing.Key{Secret: secret}, provided)
}

// TimestampWithinWindow 判 X-Timestamp 是否落在 ±Window 之内。
//
// 与老系统（ValidateTimestamp）的差别有两条，都是收紧：
//
//   - 老系统只判 `now - timestamp > 300`，也就是**只挡过去**。一个来自未来的时间戳
//     （客户端时钟快一小时）照样通过，而它配合 nonce 去重意味着那个 nonce 要在 Redis 里
//     活到那个未来时刻——去重的有效期被请求自己拉长了。这里是双向窗口。
//   - 老系统把「格式错」与「超窗」分成两个错误码回给调用方。这里都只是 error_code，
//     对外的响应是同一句话（见包注释）。
//
// 返回值不是 bool 而是 error：call log 要记下**为什么**拒的（格式错 vs 超窗），而这两件事
// 在排查时的指向完全不同——前者是对方的实现问题，后者多半是两侧时钟没同步。
func TimestampWithinWindow(raw string, now time.Time) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("missing %s header", HeaderTimestamp)
	}
	seconds, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not a unix second: %w", HeaderTimestamp, err)
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew > Window {
		return fmt.Errorf("timestamp is %s in the past, window is %s", skew.Round(time.Second), Window)
	}
	if skew < -Window {
		return fmt.Errorf("timestamp is %s in the future, window is %s", (-skew).Round(time.Second), Window)
	}
	return nil
}

// signedPath 返回进待签串的路径。
//
// 用 EscapedPath() 而不是 Path：后者会把 %2F 解成 /，于是一份签在 `/a%2Fb` 上的签名会与
// `/a/b` 的签名相等——同一个签名对应两条不同的路由。今天 /v1/openapi 下没有需要转义的段，
// 但「今天没有」不是一条能被将来的路由继承的性质。
//
// EscapedPath() 在「路径里没有需要转义的字符」时返回的就是 Path 本身，所以合作方按文档签
// 一个普通路径时，两种写法得到同一个串。
func signedPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	if escaped := u.EscapedPath(); escaped != "" {
		return escaped
	}
	return u.Path
}
