package ingress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/signing"
)

// ============================================================
// 老系统那份实现：**逐字抄来**，不改一个字符
// ============================================================
//
// 来源：panda_serve/internal/app/openapi/services/merchant_api_service.go:99（GenerateSignature）
// 与 internal/app/openapi/handlers/merchant_api_handler.go:622（buildSignatureParams）。
//
// # 为什么要在测试里抄一份
//
// 「我们与老系统的签名算法是同一条』是本轮改动里唯一一句无法用单测自证的话——它说的是两个
// 代码库之间的关系，而我们不允许改老系统、也不允许在那边跑任何东西。所以把它变成一份**可
// 执行的对照**：同一份请求，两边各算一遍，比对 hex。抄写在测试里是有意的，它让「老系统改
// 了而这边的复刻没跟上」不至于发生——那份代码已经冻结了，而这份抄件就摆在对面。
//
// 三处**有意的不一致**（见 spec.go 与方案 §1.2），由后面三个专门的测试各钉一条：
//  1. 不过滤空值（老系统遍历全部键）vs V2 的 SkipEmpty。
//  2. 没有 nonce 这个参数（V2 多一个，且它进待签串）。
//  3. timestamp 先解析成 int64 再重新格式化（V2 用头里的原串）。
func legacyIsPost(r *http.Request) bool { return r.Method == http.MethodPost }

// legacyBuildSignatureParams 逐字抄自 merchant_api_handler.go:622，只把 gin 的取值换成
// *http.Request，以及把「时间是 int64 入参」照原样保留（老系统是在中间件里解析好再传进来的）。
func legacyBuildSignatureParams(r *http.Request, timestamp int64) map[string]string {
	params := make(map[string]string)

	// 添加时间戳
	params["timestamp"] = strconv.FormatInt(timestamp, 10)

	// 添加请求路径
	params["path"] = r.URL.Path

	// 添加请求方法
	params["method"] = r.Method

	// 添加查询参数
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	// 如果是POST请求，添加请求体参数
	if legacyIsPost(r) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil && len(bodyBytes) > 0 {
			var body map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &body); err == nil {
				for key, value := range body {
					if str, ok := value.(string); ok {
						params[key] = str
					} else {
						// 将其他类型转换为字符串
						if bytes, err := json.Marshal(value); err == nil {
							params[key] = string(bytes)
						}
					}
				}
			}
		}
	}

	return params
}

// legacyGenerateSignature 逐字抄自 merchant_api_service.go:99（只少了那句 fmt.Println）。
func legacyGenerateSignature(params map[string]string, secret string) string {
	// 1. 参数排序
	var keys []string
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 2. 参数拼接
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, params[k]))
	}
	paramStr := strings.Join(pairs, "&")

	// 3. 拼接密钥
	signStr := paramStr + "&secret=" + secret

	// 4. HMAC-SHA256签名
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(signStr))
	return strings.ToLower(hex.EncodeToString(h.Sum(nil)))
}

// legacyValidateTimestamp 逐字抄自 merchant_api_service.go:125。测试用它是为了钉住
// 「V2 双向窗口、老系统只挡过去」这一条收紧。
func legacyValidateTimestamp(timestamp int64, now time.Time) error {
	if now.Unix()-timestamp > 300 { // 5分钟内有效
		return fmt.Errorf("时间戳无效")
	}
	return nil
}

// ============================================================
// 测试用的小工具
// ============================================================

const testSecret = "0123456789abcdef0123456789abcdef"

// partnerRequest 造一份合作方请求：头齐、时间戳规范化、body 原样。
//
// 时间戳用**规范化**的形态（没有前导零、没有空白），也就是老系统重新格式化之后与之相同的
// 那一串——这样第一组对照测的就纯粹是「拼串与摘要」这一层，而不是时间戳格式那一层。
func partnerRequest(method, target string, body []byte, timestamp int64, nonce string) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	r := httptest.NewRequest(method, target, reader)
	r.Header.Set(HeaderAPIKey, "KEY-0000000000000000000000000001")
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderSignature, "unused-by-this-test")
	return r
}

// signAsPartner 站在**合作方**那一侧算一个签名：它只有 api_key 与 secret，没有任何我们的
// 内部结构。所以它自己拼一遍串——用 Spec() 暴露出来的那份规则，而不是调 Verify。
func signAsPartner(t *testing.T, r *http.Request, body []byte, secret string) string {
	t.Helper()
	signature, err := signing.Sign(Params(r, body), Spec(), signing.Key{Secret: secret})
	if err != nil {
		t.Fatalf("sign as partner: %v", err)
	}
	return signature
}

// ============================================================
// 与老系统的对照
// ============================================================

// TestCanonicalAgreesWithLegacyOnTheSharedParams 是本轮最重要的一条对照：同一份请求、同一个
// 密钥，老系统的 GenerateSignature 与 V2 的 signing.Sign 必须产出**逐字节相同**的 hex。
//
// 抄件里的参数集与 V2 的参数集差一个 nonce（见下一条测试），所以这里把它摘掉再比——摘掉之后
// 两份 map 应当完全相同：键名（timestamp / path / method + query + body）、取值规则（字符串
// 原样、其余 json.Marshal）、排序（sort.Strings 逐字节升序）、追加密钥的写法（"&secret="）
// 与摘要（HMAC-SHA256 小写 hex）全部逐项对齐。
func TestCanonicalAgreesWithLegacyOnTheSharedParams(t *testing.T) {
	const timestamp = 1700000000
	body := []byte(`{"userId":"9f1d1c22-6a1e-4a0e-9f6f-0f5b6a2c1111","amount":12,"nested":{"b":2}}`)
	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement?storeId=s-1&page=2",
		body, timestamp, "nonce-0001")

	// V2 侧：摘掉 nonce（它是 V2 独有的，见下一个测试），其余原样。
	ours := Params(r, body)
	delete(ours, "nonce")
	oursHex, err := signing.Sign(ours, Spec(), signing.Key{Secret: testSecret})
	if err != nil {
		t.Fatalf("sign with the v2 spec: %v", err)
	}

	// 老系统侧：把它自己的 body 读一遍（抄件会消费 r.Body，所以另起一份请求）。
	legacyRequest := partnerRequest(http.MethodPost,
		"https://partner.example.com/v1/openapi/member-price-entitlement?storeId=s-1&page=2",
		body, timestamp, "nonce-0001")
	legacyHex := legacyGenerateSignature(legacyBuildSignatureParams(legacyRequest, timestamp), testSecret)

	if oursHex != legacyHex {
		t.Fatalf("签名与老系统不一致:\n  v2   = %s\n  legacy = %s\n  v2 待签串   = %q\n  legacy 参数 = %v",
			oursHex, legacyHex, Canonical(ours, testSecret), legacyBuildSignatureParamsForLog(body, timestamp, r))
	}
}

// legacyBuildSignatureParamsForLog 只在失败时用来打印老系统那一份参数集。它不参与断言，
// 所以允许它读不出 body（返回 nil 也只影响那条日志的可读性）。
func legacyBuildSignatureParamsForLog(body []byte, timestamp int64, r *http.Request) map[string]string {
	replica := partnerRequest(r.Method, r.URL.String(), body, timestamp, r.Header.Get(HeaderNonce))
	return legacyBuildSignatureParams(replica, timestamp)
}

// TestNonceIsInTheSignedString 钉住 V2 相对老系统多出来的那个参数。
//
// 它必须在待签串里，理由写在 spec.go 的 Params 注释里（不进串的 nonce 等于一个可被重放的
// 请求只要换个 nonce 就能过）。这条测试把「它在串里」变成一个可执行的事实：改一个 nonce，
// 签名就变了。
func TestNonceIsInTheSignedString(t *testing.T) {
	const timestamp = 1700000000
	body := []byte(`{"userId":"u-1"}`)

	first := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement",
		body, timestamp, "nonce-aaaa")
	second := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement",
		body, timestamp, "nonce-bbbb")

	firstParams := Params(first, body)
	secondParams := Params(second, body)

	if firstParams["nonce"] == "" || secondParams["nonce"] == "" {
		t.Fatal("nonce 没有进参数集")
	}
	if firstParams["nonce"] != "nonce-aaaa" {
		t.Fatalf("nonce 应当原样进待签串，得到 %q", firstParams["nonce"])
	}
	if Canonical(firstParams, testSecret) == Canonical(secondParams, testSecret) {
		t.Fatal("换了 nonce 之后待签串没变——nonce 没进串，重放只要换个 nonce 就能过")
	}
}

// TestSignatureDiffersFromLegacyOnEmptyValues 钉住第一条有意收紧：**过滤空值**。
//
// 老系统遍历全部键（空值贡献 "k="），V2 的 SkipEmpty 把它丢掉。差别在 `?a=` 与 `?a` 同时
// 出现在一条 URL 上时是可观察的——那正是这里构造的输入。
//
// 老系统那边**不是**错的：它只是没管这件事。收紧的理由写在 spec.go 的 spec 变量上：空值
// 对「报文有没有被改」没有任何帮助，却让两种写法有了可观察的差别。
func TestSignatureDiffersFromLegacyOnEmptyValues(t *testing.T) {
	const timestamp = 1700000000
	target := "https://partner.example.com/v1/openapi/member-price-entitlement?userId=u-1&extra=&keep=1"

	r := partnerRequest(http.MethodPost, target, []byte(`{"a":"1"}`), timestamp, "nonce-1")
	legacy := partnerRequest(http.MethodPost, target, []byte(`{"a":"1"}`), timestamp, "nonce-1")

	ours := Params(r, []byte(`{"a":"1"}`))
	delete(ours, "nonce")
	// 注意过滤发生在**拼串那一步**（spec.Canonical 的 SkipEmpty），不在 Params 里：Params 交出
	// 的是原始的键值对，而 Canonical 才是「什么进串」的判据。所以这里断言的是串，不是 map。
	if strings.Contains(Canonical(ours, testSecret), "extra=") {
		t.Fatalf("空值参数 extra 不该进 V2 的待签串: %q", Canonical(ours, testSecret))
	}
	legacyParams := legacyBuildSignatureParams(legacy, timestamp)
	if got, ok := legacyParams["extra"]; !ok || got != "" {
		t.Fatalf("老系统的抄件应当保留空值参数（否则这条对照测的不是它）: %v", legacyParams)
	}

	oursHex, err := signing.Sign(ours, Spec(), signing.Key{Secret: testSecret})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if oursHex == legacyGenerateSignature(legacyParams, testSecret) {
		t.Fatal("带空值参数的请求在 V2 与老系统上签出了同一个串——SkipEmpty 没生效")
	}
	// 反面：不空的时候两边仍然一致（上面那条测试），所以这条差异的边界就是「值为空」。
}

// TestTimestampFormatIsTakenVerbatimUnlikeLegacy 钉住第三条差异：X-Timestamp 的**格式**。
//
// 老系统先 strconv.ParseInt 再 FormatInt 回字符串，所以一个带前导零的头（"01700000000"）
// 在它的待签串里是 "1700000000"。V2 用头里的原串（见 TimestampWithinWindow 与
// params["timestamp"]）：合作方签的是它**发出来的那一串**。
//
// 对正常的客户端（发出来就是规范十进制）两者一致——上面那条对照测的就是这种情况。差异只在
// 一条非规范的头上有观察值，而那种头在老系统上要过一个 ParseInt 才能通过校验，本来就不是
// 正常客户端的产物。这里把边界钉住，免得将来有人「顺手」把它归一化掉：归一化会让一个已经
// 按原串签好名的客户端突然全部失败。
func TestTimestampFormatIsTakenVerbatimUnlikeLegacy(t *testing.T) {
	const timestamp = 1700000000
	const raw = "01700000000" // 前导零：ParseInt 认，但不是规范形态
	body := []byte(`{"userId":"u-1"}`)

	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement",
		body, timestamp, "nonce-1")
	r.Header.Set(HeaderTimestamp, raw)

	if err := TimestampWithinWindow(raw, time.Unix(timestamp, 0)); err != nil {
		t.Fatalf("带前导零的时间戳应当能通过校验（它就是 ParseInt 认得的形式）: %v", err)
	}

	ours := Params(r, body)
	if ours["timestamp"] != raw {
		t.Fatalf("V2 应当使用头里的原串 %q，得到 %q", raw, ours["timestamp"])
	}

	legacyParams := legacyBuildSignatureParams(
		partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement",
			body, timestamp, "nonce-1"), timestamp)
	if legacyParams["timestamp"] != strconv.FormatInt(timestamp, 10) {
		t.Fatalf("老系统的抄件应当重新格式化时间戳: %v", legacyParams["timestamp"])
	}
}

// TestTimestampWindowIsTwoSidedUnlikeLegacy 钉住第二条收紧：老系统只挡过去。
func TestTimestampWindowIsTwoSidedUnlikeLegacy(t *testing.T) {
	now := time.Unix(1700000000, 0)

	const oneHour = 3600
	// 未来一小时：老系统放行（now - ts 是负数，不大于 300），V2 拒绝。
	future := now.Add(time.Hour)
	if err := legacyValidateTimestamp(future.Unix(), now); err != nil {
		t.Fatalf("老系统应当放行来自未来的时间戳（这条对照的前提不成立）: %v", err)
	}
	if err := TimestampWithinWindow(strconv.FormatInt(future.Unix(), 10), now); err == nil {
		t.Fatal("V2 必须拒绝来自未来的时间戳：配合 nonce 去重，那个 nonce 要在 Redis 里活到那个未来时刻")
	}

	// 过去一小时：两边都拒。
	past := now.Add(-time.Hour)
	if err := legacyValidateTimestamp(past.Unix(), now); err == nil {
		t.Fatal("老系统应当拒绝一小时前的时间戳（这条对照的前提不成立）")
	}
	if err := TimestampWithinWindow(strconv.FormatInt(past.Unix(), 10), now); err == nil {
		t.Fatal("V2 必须拒绝超窗的时间戳")
	}
}

// ============================================================
// 待签串本身
// ============================================================

func TestParamsCoversHeadersPathQueryAndBody(t *testing.T) {
	body := []byte(`{"userId":"u-1","count":3,"flag":true,"obj":{"a":1},"arr":[1,"x"]}`)
	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/x?a=1&b=2", body, 1700000000, "n-1")

	params := Params(r, body)

	want := map[string]string{
		"timestamp": "1700000000",
		"path":      "/v1/openapi/x",
		"method":    "POST",
		"nonce":     "n-1",
		"a":         "1",
		"b":         "2",
		"userId":    "u-1",
		// 非字符串值走 json.Marshal：这一条与老系统逐字相同（见 FlattenBody）。
		"count": "3",
		"flag":  "true",
		"obj":   `{"a":1}`,
		"arr":   `[1,"x"]`,
	}
	if len(params) != len(want) {
		t.Fatalf("参数集大小 %d，期望 %d: %v", len(params), len(want), params)
	}
	for key, value := range want {
		if params[key] != value {
			t.Fatalf("参数 %q 应当是 %q，得到 %q", key, value, params[key])
		}
	}
}

// TestParamsBodyOverridesQuery 钉住覆盖顺序（先 query 后 body，与老系统同一个顺序）。
func TestParamsBodyOverridesQuery(t *testing.T) {
	body := []byte(`{"userId":"from-body"}`)
	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/x?userId=from-query", body, 1700000000, "n-1")

	if got := Params(r, body)["userId"]; got != "from-body" {
		t.Fatalf("body 应当覆盖 query 的同名键，得到 %q", got)
	}
}

// TestBodyIsFlattenedForEveryMethodUnlikeLegacy 钉住第四条差异（比老系统**严**）：body 进串
// 不看方法是 POST。
//
// 老系统那一句是 `if c.Request.Method == "POST"`，于是 GET / PUT / PATCH / DELETE 的 body
// 完全在签名之外——一个中间人改掉它，签名照样对得上。V2 对每个方法都打平（方案 §1.2 的参数
// 清单里也没有按方法限定）。
//
// 开放接口树上今天已经有了一条 POST（设备回执，见 routes/openapi.go），所以这条差异**已经
// 能被观察到**——不再是「将来加写接口时」。一个照抄老系统示例代码、把 body 打平限定在 POST 上
// 的客户端，会在 PUT/PATCH 上验签失败，而那时的第一反应会是「我们的签名算法是不是有问题」；
// 这条测试就是为了让那个人先找到这里。
func TestBodyIsFlattenedForEveryMethodUnlikeLegacy(t *testing.T) {
	body := []byte(`{"userId":"u-1"}`)
	r := partnerRequest(http.MethodGet, "https://partner.example.com/v1/openapi/x?a=1", body, 1700000000, "n-1")

	params := Params(r, body)
	if params["userId"] != "u-1" {
		t.Fatalf("V2 对每个方法都打平 body，GET 的 body 也应当在串里: %v", params)
	}

	legacyParams := legacyBuildSignatureParams(
		partnerRequest(http.MethodGet, "https://partner.example.com/v1/openapi/x?a=1", body, 1700000000, "n-1"), 1700000000)
	if _, ok := legacyParams["userId"]; ok {
		t.Fatalf("老系统的抄件只对 POST 打平 body（这条对照的前提不成立）: %v", legacyParams)
	}
}

// TestFlattenBodyOnUnparseableJSONIsEmpty 钉住一条已知取舍：非法 JSON 的 body 贡献零个参数。
//
// 照这样处理之后，一次非法 JSON 的请求仍然**可签**（待签串里只有头与 query），所以它不会在
// 验签上「意外通过」——签名的期望值就是照这个空集算的。它会照常被后面的解析拒掉（400）。
func TestFlattenBodyOnUnparseableJSONIsEmpty(t *testing.T) {
	if got := FlattenBody([]byte(`{not json`)); len(got) != 0 {
		t.Fatalf("非法 JSON 应当打平出空集，得到 %v", got)
	}
	if got := FlattenBody(nil); len(got) != 0 {
		t.Fatalf("空 body 应当打平出空集，得到 %v", got)
	}
	// 顶层不是对象（数组、标量）同样解不成 map —— 走的是同一条路。
	if got := FlattenBody([]byte(`[1,2,3]`)); len(got) != 0 {
		t.Fatalf("顶层数组没有可打平的键，得到 %v", got)
	}
}

// ============================================================
// 验签本身
// ============================================================

func TestVerifyAcceptsAPartnerSignatureAndRejectsTampering(t *testing.T) {
	const timestamp = 1700000000
	body := []byte(`{"userId":"u-1","amount":10}`)
	target := "https://partner.example.com/v1/openapi/member-price-entitlement?storeId=s-1"

	sign := func(r *http.Request, b []byte) string { return signAsPartner(t, r, b, testSecret) }

	base := partnerRequest(http.MethodPost, target, body, timestamp, "n-1")
	signature := sign(base, body)
	if err := Verify(Params(base, body), testSecret, signature); err != nil {
		t.Fatalf("合作方按 V2 规则签出来的签名应当通过: %v", err)
	}

	// 失败矩阵：每一个字段被改一下，都必须拒。
	cases := []struct {
		name string
		// 造一份被改过的请求（与签名的原请求只差一个地方）
		mutate func(*http.Request) *http.Request
		body   []byte
	}{
		{"body 改了一个字节", func(r *http.Request) *http.Request { return r }, []byte(`{"userId":"u-1","amount":11}`)},
		{"query 改了", func(r *http.Request) *http.Request {
			return partnerRequest(http.MethodPost, target+"&extra=1", body, timestamp, "n-1")
		}, body},
		{"path 改了", func(r *http.Request) *http.Request {
			return partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/other?storeId=s-1", body, timestamp, "n-1")
		}, body},
		{"method 改了", func(r *http.Request) *http.Request {
			return partnerRequest(http.MethodPut, target, body, timestamp, "n-1")
		}, body},
		{"nonce 改了", func(r *http.Request) *http.Request {
			return partnerRequest(http.MethodPost, target, body, timestamp, "n-2")
		}, body},
		{"timestamp 改了", func(r *http.Request) *http.Request {
			return partnerRequest(http.MethodPost, target, body, timestamp+1, "n-1")
		}, body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(base)
			err := Verify(Params(mutated, tc.body), testSecret, signature)
			if err == nil {
				t.Fatal("签名应当不通过")
			}
			if !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("应当是签名不匹配（不是密钥没配那一类），得到: %v", err)
			}
		})
	}

	// 换一把密钥（伪造者手上没有真的那一把）。
	if err := Verify(Params(base, body), "another-secret-of-the-same-length!", signature); err == nil {
		t.Fatal("用另一把密钥算出来的签名必须不通过")
	}
}

// TestVerifyRefusesAnEmptySecret 钉住「空密钥绝不等于放行」。
//
// 能走到这里的密钥来自库里的密文信封，而它解出来是空串只可能是**数据坏了**。放行的后果是
// 任何签名都通过（空密钥 + 空签名），所以它必须是拒绝，且错误要是「没配密钥」那一类
// （让调用方与「签名不对」分开处置）。
func TestVerifyRefusesAnEmptySecret(t *testing.T) {
	body := []byte(`{"userId":"u-1"}`)
	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/x", body, 1700000000, "n-1")

	for _, empty := range []string{"", "   "} {
		if err := Verify(Params(r, body), empty, ""); err == nil {
			t.Fatal("空密钥必须拒绝，绝不能因为签名的期望值也是空串就放行")
		}
	}
}

func TestTimestampWithinWindow(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// 错误里要出现的一个片段：格式错与超窗在排查时的指向完全不同（前者是对方的实现问题，
		// 后者多半是两侧时钟没同步），而调用日志里记的就是这个 error。
		wantFragment string
	}{
		{"正好在窗口的过去边界上", strconv.FormatInt(now.Add(-Window).Unix(), 10), false, ""},
		{"正好在窗口的未来边界上", strconv.FormatInt(now.Add(Window).Unix(), 10), false, ""},
		{"窗口外一秒（过去）", strconv.FormatInt(now.Add(-Window-time.Second).Unix(), 10), true, "past"},
		{"窗口外一秒（未来）", strconv.FormatInt(now.Add(Window+time.Second).Unix(), 10), true, "future"},
		{"空", "", true, "missing"},
		{"只有空白", "   ", true, "missing"},
		{"不是十进制", "17e9", true, "not a unix second"},
		{"带小数的秒", "1700000000.5", true, "not a unix second"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := TimestampWithinWindow(tc.raw, now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应当报错")
				}
				if !strings.Contains(err.Error(), tc.wantFragment) {
					t.Fatalf("错误里应当出现 %q，得到 %v", tc.wantFragment, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应当报错: %v", err)
			}
		})
	}
}

// TestSignedPathIsEscapedPath 钉住用 EscapedPath 而不是 Path：%2F 不能塌成 /。
func TestSignedPathIsEscapedPath(t *testing.T) {
	body := []byte(`{}`)
	escaped := partnerRequest(http.MethodGet, "https://partner.example.com/v1/openapi/a%2Fb", body, 1700000000, "n-1")
	plain := partnerRequest(http.MethodGet, "https://partner.example.com/v1/openapi/a/b", body, 1700000000, "n-1")

	escapedParams := Params(escaped, body)
	plainParams := Params(plain, body)
	if escapedParams["path"] == plainParams["path"] {
		t.Fatalf("两条不同的路由签出了同一个 path (%q)——%%2F 被当成了 /", escapedParams["path"])
	}
	if !strings.Contains(escapedParams["path"], "%2F") {
		// 注意 %2F 本身要写成 %%2F：这里是一句格式串。
		t.Fatalf("转义路径应当原样进串（%%2F 不能被解开），得到 %q", escapedParams["path"])
	}
}

// ============================================================
// IP 白名单
// ============================================================

func TestParseIPAllowListAcceptsCIDRsAndBareAddresses(t *testing.T) {
	list, err := ParseIPAllowList([]string{" 10.0.0.0/8 ", "203.0.113.7", "2001:db8::1", "::1/128"})
	if err != nil {
		t.Fatalf("这些都应当被接受: %v", err)
	}
	// 裸地址按 /32（IPv4）或 /128（IPv6）收窄，所以 2001:db8::1 只放行它自己。
	for _, allowed := range []string{"10.1.2.3", "203.0.113.7", "2001:db8::1", "::1"} {
		if !list.Allows(allowed) {
			t.Fatalf("%s 在白名单里", allowed)
		}
	}
	for _, denied := range []string{"11.0.0.1", "203.0.113.8", "2001:db8::99", "2001:db9::1", "192.168.1.1"} {
		if list.Allows(denied) {
			t.Fatalf("%s 不在白名单里", denied)
		}
	}
	// 带端口的地址（RemoteAddr 的形状）也要认。
	if !list.Allows("10.1.2.3:54321") {
		t.Fatal("带端口的地址应当能判定")
	}
}

// TestEmptyAllowListMeansUnrestricted 钉住「空数组 ≠ 全拒」。
//
// 新签发的密钥默认不限制来源：反过来的话，运营不填白名单就调不通，而报出来的是一句 401
// ——他会先去怀疑签名算错了。这条语义写在 migrations/partner 那一列上。
func TestEmptyAllowListMeansUnrestricted(t *testing.T) {
	for _, entries := range [][]string{nil, {}, {"", "   "}} {
		list, err := ParseIPAllowList(entries)
		if err != nil {
			t.Fatalf("%v 应当被接受为空: %v", entries, err)
		}
		if list != nil {
			t.Fatalf("%v 应当归一成 nil", entries)
		}
		if !list.Allows("8.8.8.8") {
			t.Fatal("没配白名单时应当放行一切")
		}
		if !list.Allows("") {
			t.Fatal("没配白名单时连解析不出来的地址也放行（nil 接收者不看地址）")
		}
	}
}

// TestMalformedAllowListFailsTheWholeList 钉住「有一条不合法就整份失败」。
//
// 写错一项的后果是那个来源被静默拒掉（或更糟：被静默放行），而两种都只在生产上表现为
// 「某台机器突然调不通」。所以它必须在写入口就被拒（见 service.parseKeyAccess）。
func TestMalformedAllowListFailsTheWholeList(t *testing.T) {
	for _, entries := range [][]string{
		{"10.0.0.0/8", "not-an-ip"},
		{"10.0.0.0/33"},
		{"http://example.com"},
	} {
		if _, err := ParseIPAllowList(entries); err == nil {
			t.Fatalf("%v 里有一条不合法，整份都该失败", entries)
		}
	}
}

// TestAllowListRejectsUnparseableAddress 钉住「地址解析不出来时拒绝」。
//
// resolveClientIP 交给我们的串只有两种来路（RemoteAddr 解析出来的 host，或可信代理写的 XFF
// 的某一跳）。一个解析不出来的值说明链路里有人在写畸形地址——那种情况下放行等于把白名单
// 交给对方决定。
func TestAllowListRejectsUnparseableAddress(t *testing.T) {
	list, err := ParseIPAllowList([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, address := range []string{"", "   ", "not-an-ip", "10.0.0.1/8"} {
		if list.Allows(address) {
			t.Fatalf("%q 解析不出来，必须拒绝", address)
		}
	}
}
