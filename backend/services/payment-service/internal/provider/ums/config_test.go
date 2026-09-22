package ums

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一组测试守的是同一件事：**我们的签名与老系统逐字节相同**。
//
// 判据不是「自己签自己验」（那永远是对的），而是下面 legacyAuthHeader 这十来行——它是
// panda_serve/internal/app/miniapp/services/umspay_service.go:225 那段 buildUMSAuthHeader 的
// 逐字复刻，独立于本包的 Sign 而存在。老系统**只读对照，没有改**。
//
// 迁移时对不上账的根都在这里：一个引号、一个空格、摘要的大小写、base64 与 hex 之误，都会让
// 渠道回一句「签名错误」，而两边打印出来的报文看上去一模一样。

// legacyAuthHeader 是 panda_serve 那份实现的逐字复刻。
//
// 唯一的不同是 timestamp 与 nonce 由调用方给：老系统在函数内部取当前时间（`time.Now()` 与
// `time.Now().UnixNano()`），那样没法做断言。换掉那两行之后，其余每一行、每一个字符都与
// 老系统相同——包括 `fmt.Sprintf("%x", …)` 出来的**小写** hex 摘要，以及那句四段式的 Sprintf。
//
// 老系统的 hmacSHA256Base64 返回 (string, error)，这里忽略 error：唯一的失败源是 mac.Write
// 对 []byte 的写入，它永远不会失败。
func legacyAuthHeader(appID, appKey, timestamp, nonce string, body []byte) string {
	return fmt.Sprintf("OPEN-BODY-SIG AppId=\"%s\", Timestamp=\"%s\", Nonce=\"%s\", Signature=\"%s\"",
		appID, timestamp, nonce, legacySignature(appID, appKey, timestamp, nonce, body))
}

// legacySignature 只到裸签名那一层，不拼头。
//
// 拆出来是因为 **H5 那条路（OPEN-FORM-PARAM）用的是同一套签名算法**，差别只在参数往哪儿放：
// 那一套装在 Authorization 头里、这一套进查询串。两处共用这一份复刻，验的就都是「我们算的
// 是不是老系统/规范那一套」，而不是「我们跟自己是不是一致」。
func legacySignature(appID, appKey, timestamp, nonce string, body []byte) string {
	strToSign := appID + timestamp + nonce + legacySHA256Hex(body)
	mac := hmac.New(sha256.New, []byte(appKey))
	mac.Write([]byte(strToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// legacySHA256Hex 是 umspay_service.go:238 的 sha256Hex 的逐字复刻。
func legacySHA256Hex(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// credentialsFor 把测试密钥装进这一族的槽里。
//
// 槽名 `appKey` 与 channelConfig 里 sign.secretRef 填的是同一个——适配器就是照配置里那个
// 名字取的（见 TestSecretSlotComesFromConfig）。对不上时取出来是空串、调用会被本地拒，
// 测试立刻红。
func credentialsFor(secret string) provider.Credentials {
	return provider.Credentials{"appKey": secret}
}

const (
	testAppID  = "ums-test-app"
	testSecret = "8f3a1c6e9b2d4705af81c3e6d29b7f04"
	testMid    = "M0001"
	testTid    = "T0001"
	testSource = "3CYM"
	testDomain = "www.example.com"
)

// channelConfig 是一份能过 Parse 的声明。老系统 UMSPayService 的五个字段一一对上，
// appKey 走凭据槽（老系统把它明文放在结构体上）。
func channelConfig(baseURL string) map[string]any {
	return map[string]any{
		"baseURL":    baseURL,
		"appId":      testAppID,
		"mid":        testMid,
		"tid":        testTid,
		"sourceCode": testSource,
		"domainName": testDomain,
		"sign":       map[string]any{"secretRef": "appKey"},
	}
}

func channelMethod(config map[string]any) provider.Method {
	return provider.Method{
		// 渠道名（catalog.ChannelCodeUMS 的那个值）。这里写字面量而不是引 catalog：那个包
		// 反过来 import 本包，引用它就是环。
		ChannelCode: "ums",
		Provider:    Name,
		// action 是**分派依据**（见 create.go 的 switch）：这一族的小程序那条路是 native_pay。
		// 老系统里 ums 只有这一条路，所以测试默认走它。
		Action:        provider.ActionNativePay,
		ChannelConfig: config,
	}
}

// TestSignMatchesLegacyImplementation 是这一族的地基。
func TestSignMatchesLegacyImplementation(t *testing.T) {
	body := []byte(`{"merOrderId":"PAY20260917000000000001"}`)
	const (
		timestamp = "20260917102030"
		nonce     = "1767225600123456789"
	)

	got := authorizationHeader(testAppID, timestamp, nonce,
		Sign(testAppID, timestamp, nonce, body, testSecret))
	want := legacyAuthHeader(testAppID, testSecret, timestamp, nonce, body)
	if got != want {
		t.Fatalf("与老系统算出来的签名头不同：\n got %s\nwant %s", got, want)
	}

	// 再钉一个**写死的**向量。上面那条要求两边同时改错才会过——写死的这个不会：它把
	// 「时间戳怎么拼、摘要是 hex 还是 base64、大小写如何」全部固定下来，改任何一处都会红。
	//
	// 这个 base64 串是用 Python 的 hmac/hashlib/base64 独立算出来的（同样的四个入参、同样的
	// 密钥），不是从本包的 Sign 读回来的：从自己读回来的常量什么都证明不了。
	const pinnedSignature = "DYrvVYK02yaD7KbTH/PhIS9W4LeQq9VrpqNSpYy28xM="
	if signature := Sign(testAppID, timestamp, nonce, body, testSecret); signature != pinnedSignature {
		t.Fatalf("签名与钉死的向量不同：got %s, want %s", signature, pinnedSignature)
	}
	const pinnedHeader = `OPEN-BODY-SIG AppId="ums-test-app", Timestamp="20260917102030", Nonce="1767225600123456789", Signature="DYrvVYK02yaD7KbTH/PhIS9W4LeQq9VrpqNSpYy28xM="`
	if got != pinnedHeader {
		t.Fatalf("签名头与钉死的向量不同：\n got %s\nwant %s", got, pinnedHeader)
	}
}

// TestPayloadShape 把待签串本身钉下来：四段、无分隔符、末段是**报文体**的 sha256 小写 hex。
//
// 单独测它而不是只测 Sign：多渠道联调时，运维与渠道方对着看的就是这一段串（后台的「试跑」
// 也把它显示出来），所以它的形状本身是一份对外契约。注意它**没有分隔符**——照抄老系统的
// `s.AppID + timestamp + nonce + bodyDigest`，往里加一个 "&" 会让签名对不上。
func TestPayloadShape(t *testing.T) {
	body := []byte(`{"a":1}`)
	payload := Payload("app", "123", "n", body)

	if !strings.HasPrefix(payload, "app123n") {
		t.Fatalf("待签串的前三段不对（且必须无分隔符）：%q", payload)
	}
	if !strings.HasSuffix(payload, legacySHA256Hex(body)) {
		t.Fatalf("待签串的末段该是报文体的 sha256：%q", payload)
	}
	// **碰一下报文体，整串就该变**——这正是「报文摘要进签名」买到的东西。
	if Payload("app", "123", "n", []byte(`{"a":2}`)) == payload {
		t.Fatal("改了报文体而待签串没变：报文摘要没进签名")
	}
	// appId 也在待签串里：换个 appId 签出来的必须不同（验签时我们靠先把 appId 钉死挡这个）。
	if Payload("another", "123", "n", body) == payload {
		t.Fatal("换了 appId 而待签串没变：appId 没进签名")
	}
}

// TestParseDefaults：所有默认值都照抄老系统写死的那几处，改一个都打不着渠道。
func TestParseDefaults(t *testing.T) {
	protocol, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct{ name, got, want string }{
		{"createPath", protocol.createPath, defaultCreatePath},
		{"queryPath", protocol.queryPath, defaultQueryPath},
		{"instMid", protocol.instMid, defaultInstMid},
		{"tradeType", protocol.tradeType, defaultTradeType},
		{"secretRef", protocol.secretRef, "appKey"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q, want %q（默认值照抄老系统）", tc.name, tc.got, tc.want)
		}
	}
	if protocol.returnURL != "" {
		t.Fatalf("returnUrl 是可选的，没配该是空串，实际 %q", protocol.returnURL)
	}
}

// TestParseRequiresThePinnedFields：六个必填项，缺一个都要点名**完整路径**。
//
// baseURL 不在这一列里，它是**唯一一个不填就用默认值**的账户值（默认值就是生产地址，见
// defaultBaseURL 的说明）——所以它由下面那条用例单独验。
//
// sourceCode / domainName 单独立两条是因为它们的失败方式最贵：老系统硬编码的是**别家主体**的
// 登记值，给它们一个默认值等于让没登记的渠道拿着别人的来源编号把报文发出去。
func TestParseRequiresThePinnedFields(t *testing.T) {
	cases := []struct {
		name   string
		damage func(map[string]any)
		path   string
	}{
		{"没有 appId", func(c map[string]any) { delete(c, "appId") }, "config.appId"},
		{"没有 mid", func(c map[string]any) { delete(c, "mid") }, "config.mid"},
		{"没有 tid", func(c map[string]any) { delete(c, "tid") }, "config.tid"},
		{"没有 sourceCode", func(c map[string]any) { delete(c, "sourceCode") }, "config.sourceCode"},
		{"没有 domainName", func(c map[string]any) { delete(c, "domainName") }, "config.domainName"},
		{"没有 secretRef", func(c map[string]any) { delete(c["sign"].(map[string]any), "secretRef") }, "config.sign.secretRef"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := channelConfig("https://api-mop.chinaums.com")
			tc.damage(config)

			_, err := Parse(config)
			if !errors.Is(err, provider.ErrConfigInvalid) {
				t.Fatalf("want ErrConfigInvalid, got %v", err)
			}
			// 错误串要给出**完整路径**：配置是一棵树，只说「没填」会让人去别的段里找。
			if !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("错误串该点名 %s，实际 %v", tc.path, err)
			}
		})
	}
}

// TestParseFallsBackToTheProductionBaseURL：没配 baseURL 时用生产地址，而不是报错。
//
// 这与上面六项的区别是**有无一个正确的取值**：appId / mid / tid 那些是这一家商户的登记值，
// 猜不出来，空着只能拒；而根地址在「不联调」的时候就是那一个。
func TestParseFallsBackToTheProductionBaseURL(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	delete(config, "baseURL")

	protocol, err := Parse(config)
	if err != nil {
		t.Fatalf("没配 baseURL 不该拒绝：%v", err)
	}
	if protocol.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", protocol.baseURL, defaultBaseURL)
	}
}

// TestURLEndsWithTheConfiguredPath：baseURL 带不带结尾斜杠拼出来的地址必须一样。
//
// 两边都带会拼出 `//v1/netpay/wx/unified-order`，有些网关把它当另一个路径、直接 404。
func TestURLEndsWithTheConfiguredPath(t *testing.T) {
	withSlash, err := Parse(channelConfig("https://api-mop.chinaums.com/"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	without, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "https://api-mop.chinaums.com/v1/netpay/wx/unified-order"
	if got := withSlash.url(defaultCreatePath); got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if got := without.url(defaultCreatePath); got != want {
		t.Fatalf("baseURL 带不带结尾斜杠拼出来的地址不同：%q", got)
	}
}

// TestOptionsOverrideFromParams 钉死「一个渠道挂三个支付方式」那一处。
//
// 差异只有三个值，而且**以 params 为准**：支付方式是运营为每一次收款挑的那个东西，config 上的
// 默认值是「这个渠道大多数情况怎么走」。空值等于没配（`""` 与「没这个键」对运营是同一件事）。
func TestOptionsOverrideFromParams(t *testing.T) {
	protocol, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	t.Run("支付宝那条支付方式", func(t *testing.T) {
		options := protocol.options(map[string]string{
			paramTradeType:  "ALIPAY",
			paramCreatePath: "/v1/netpay/trade/create",
			paramInstMid:    "ALIPAYDEFAULT",
		}, provider.ActionNativePay)
		if options.tradeType != "ALIPAY" || options.createPath != "/v1/netpay/trade/create" ||
			options.instMid != "ALIPAYDEFAULT" {
			t.Fatalf("params 没覆盖住 config 默认值：%+v", options)
		}
	})

	t.Run("没有 params 的那条", func(t *testing.T) {
		options := protocol.options(nil, provider.ActionNativePay)
		if options.tradeType != defaultTradeType || options.createPath != defaultCreatePath ||
			options.instMid != defaultInstMid {
			t.Fatalf("没给 params 时该全部落回 config 默认值：%+v", options)
		}
	})

	t.Run("只配了一个键", func(t *testing.T) {
		options := protocol.options(map[string]string{paramTradeType: "JSAPI"}, provider.ActionNativePay)
		if options.tradeType != "JSAPI" {
			t.Fatalf("tradeType = %q, want JSAPI", options.tradeType)
		}
		if options.createPath != defaultCreatePath || options.instMid != defaultInstMid {
			t.Fatalf("没配的两个键该落回默认值：%+v", options)
		}
	})

	t.Run("空串等于没配", func(t *testing.T) {
		options := protocol.options(map[string]string{paramTradeType: "  "}, provider.ActionNativePay)
		if options.tradeType != defaultTradeType {
			t.Fatalf("空串该落回默认值，实际 %q", options.tradeType)
		}
	})
}

// TestRequiresOpenIDUsesTheEffectiveTradeType：判据是**生效的** tradeType。
//
// 一条把 tradeType 配成 ALIPAY 的支付方式不该因为渠道默认是 MINI 就被要求带 openid——
// 那会让支付宝那条支付方式一条都发不出去。
func TestRequiresOpenIDUsesTheEffectiveTradeType(t *testing.T) {
	protocol, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := []struct {
		tradeType string
		want      bool
	}{
		{"MINI", true},
		{"JSAPI", true},
		// 大小写与空格无所谓：这个值在 params 与 config 两处都由人手填。
		{"mini", true},
		{" JSAPI ", true},
		{"ALIPAY", false},
		{"", false},
		{"UPCARD", false},
	}
	for _, tc := range cases {
		if got := protocol.requiresOpenID(tc.tradeType); got != tc.want {
			t.Fatalf("requiresOpenID(%q) = %v, want %v", tc.tradeType, got, tc.want)
		}
	}

	// 列表可配：一家只收支付宝的渠道可以把这一项配空，于是它一条都不要求 openid。
	empty, err := Parse(withConfig(channelConfig("https://api-mop.chinaums.com"), "openIdTradeTypes", []any{}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(empty.openIDTradeTypes) != len(defaultOpenIDTradeTypes) {
		t.Fatalf("空列表该落回默认值，实际 %v", empty.openIDTradeTypes)
	}

	custom, err := Parse(withConfig(channelConfig("https://api-mop.chinaums.com"), "openIdTradeTypes", []any{"ALIPAY"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if custom.requiresOpenID("MINI") || !custom.requiresOpenID("ALIPAY") {
		t.Fatalf("自定义列表没生效：%v", custom.openIDTradeTypes)
	}

	// 默认值那个切片是包级的，交给调用方之前必须复制——否则改一个元素就改了全进程的默认值。
	if len(defaultOpenIDTradeTypes) != 2 || defaultOpenIDTradeTypes[0] != "MINI" {
		t.Fatalf("包级默认值被改动了：%v", defaultOpenIDTradeTypes)
	}
}

// withConfig 复制一份 config 再塞一个键，避免用例之间互相污染。
func withConfig(config map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(config)+1)
	for name, item := range config {
		out[name] = item
	}
	out[key] = value
	return out
}

// TestSecretSlotComesFromConfig：凭据槽名由适配器说了算，装配处照它去解密文。
func TestSecretSlotComesFromConfig(t *testing.T) {
	method := channelMethod(channelConfig("https://api-mop.chinaums.com"))
	if got := New().SecretSlots(method); !slices.Equal(got, []string{"appKey"}) {
		t.Fatalf("SecretSlots = %q, want [appKey]", got)
	}

	// 配置坏掉时返回空切片而不是报错：装配处拿到空切片会退回渠道行的 secret_ref，
	// 而配置坏掉这件事会在紧随其后的 Create / Verify 里以一条完整得多的错误爆出来。
	broken := channelMethod(map[string]any{"baseURL": "https://api-mop.chinaums.com"})
	if got := New().SecretSlots(broken); len(got) != 0 {
		t.Fatalf("坏配置该回空切片，实际 %q", got)
	}
}
