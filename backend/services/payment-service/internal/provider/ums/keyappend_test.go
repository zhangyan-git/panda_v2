package ums

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一组测试守的是这一族的**第三套**签名：入站报文（H5 的支付结果通知与结果页回跳）的
// key 拼接签名。它既不是出站那两套中的任何一套，也与它们没有任何共用代码。
//
// 判据与 config_test.go 那组一样，不是「自己签自己验」：下面 legacyKeyAppendSignature 是
// 规范 §1.9.3 那五条规则的**独立复刻**，还另钉了一个用 Python 算出来的写死向量。两条一起
// 才说明「我们算的与规范说的是一回事」——单靠任何一条都可能两边同时错。
//
// 但有一条前提必须重复：**老系统没有验签，规范对回调的签名参数名也没写死**。所以这一组
// 证明的是「这套算法按规范的描述实现得自洽」，不是「渠道的回调真是这么签的」。后者要拿真实
// 凭据实测（见 ums.go 包注释与文档 §10.1）。

// testCommSecret 是通讯密钥的测试值，与 testSecret（appKey）**故意不同**：两把密钥在这一族
// 里的方向相反，测试若让它们相等，一处「拿 appKey 去验入站签名」的实现错误就永远暴露不出来。
const testCommSecret = "b7d2e4f60a1c35e8974d0b6f2a8c5139"

// legacyKeyAppendSignature 是规范 §1.9.3 那五条规则的逐字复刻，独立于本包的实现：
//
//  1. 参数按名字的 ASCII 字典序排序；
//  2. 用 `&` 连成 `k=v&k=v`；
//  3. 末尾**直接接上通讯密钥**（不是 HMAC、不是 base64）；
//  4. 按 signType 取 MD5 或 SHA256；
//  5. 无值的参数不参与。
//
// 第 5 条里的「签名参数自己不参与」是本包补的：规范没写，但让签名参数参与等于要求渠道算一个
// 含自己摘要的摘要。这一份复刻**故意也把它排除掉**——两边的判断一致，测的才是排序与拼接。
func legacyKeyAppendSignature(params map[string]string, secret, signType string) string {
	names := make([]string, 0, len(params))
	for name, value := range params {
		if value == "" || name == "sign" || name == "signature" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+params[name])
	}
	payload := strings.Join(pairs, "&") + secret

	if signType == signTypeMD5 {
		sum := md5.Sum([]byte(payload))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// commKeyConfig 是一份**带通讯密钥槽**的声明。H5 那条路的入站验签要第二把密钥，而 channelConfig
// 只声明了 appKey（小程序那条路的那把）。
func commKeyConfig(baseURL string) map[string]any {
	return withConfig(channelConfig(baseURL), "sign", map[string]any{
		"secretRef":     "appKey",
		"commSecretRef": "commKey",
	})
}

// commCredentials 是两把密钥都在的凭据表。
func commCredentials() provider.Credentials {
	return provider.Credentials{"appKey": testSecret, "commKey": testCommSecret}
}

// keyAppendFormBody 把参数拼成一份表单报文，并把签名塞进去。
//
// 表单的名字与值都用 url.Values.Encode 编（它按名字排序，但那正好是**报文里**的顺序，与待签串
// 的顺序无关——待签串是我们在验签时自己排的）。
func keyAppendFormBody(params map[string]string, signatureParam, signature string) []byte {
	values := url.Values{}
	for name, value := range params {
		values.Set(name, value)
	}
	if signature != "" {
		values.Set(signatureParam, signature)
	}
	return []byte(values.Encode())
}

// keyAppendCallback 造一份走 key 拼接签名的**回调**（POST，体里是表单）。
func keyAppendCallback(config map[string]any, params map[string]string, signatureParam string) provider.NotificationRequest {
	signType := params[paramSignType]
	if signType == "" {
		signType = signTypeSHA256
	}
	signature := legacyKeyAppendSignature(params, testCommSecret, signType)
	return provider.NotificationRequest{
		ChannelCode: "ums_wechat",
		Body:        keyAppendFormBody(params, signatureParam, signature),
		Headers:     http.Header{},
		HTTPMethod:  http.MethodPost,
		RequestPath: callbackPath,
		Method:      channelMethod(config),
		Secrets:     commCredentials(),
	}
}

// testCallbackParams 是一份「像真的」的回调参数：带一个随机名字的字段（规范说它参与签名）。
func testCallbackParams() map[string]string {
	return map[string]string{
		"merOrderId":  "PAY20260917000000000001",
		"status":      "TRADE_SUCCESS",
		"totalAmount": "12800",
		"notifyId":    "N-1",
		"payTime":     "2026-09-17 10:20:30",
		"signType":    signTypeSHA256,
		"rnd":         "k9Qv2mZ1",
	}
}

// TestKeyAppendSignatureMatchesTheSpec 是这一族第三套签名的地基。
func TestKeyAppendSignatureMatchesTheSpec(t *testing.T) {
	params := testCallbackParams()
	// 空值与签名参数都不参与——这两条是本包**自己**补进规范的（见 legacyKeyAppendSignature）。
	params["empty"] = ""
	params["signature"] = "SHOULD-BE-EXCLUDED"

	if got, want := keyAppendSignature(params, testCommSecret, signTypeSHA256),
		legacyKeyAppendSignature(params, testCommSecret, signTypeSHA256); got != want {
		t.Fatalf("与独立复刻的算法不同：\n got %s\nwant %s", got, want)
	}

	// 再钉一个**写死的**向量：上面那条要求两边同时改错才会过，写死的这个不会。它把排序规则、
	// 分隔符、密钥的拼接方式、摘要的种类与大小写全部固定下来。
	//
	// 这个串是用 Python 的 hashlib 独立算出来的（同一份参数、同一把密钥）。待签原文是
	// `merOrderId=…&notifyId=N-1&payTime=…&rnd=k9Qv2mZ1&signType=SHA256&status=TRADE_SUCCESS&totalAmount=12800`
	// 后面**直接**跟密钥——注意 `totalAmount=12800b7d2…` 那里没有分隔符，那正是规则第 3 条。
	const pinnedSHA256 = "3a67f4f883d8086f2575e5da38a6820c3d45b104846c3927017f56b056a40bf1"
	if got := keyAppendSignature(params, testCommSecret, signTypeSHA256); got != pinnedSHA256 {
		t.Fatalf("SHA256 签名与钉死的向量不同：got %s, want %s", got, pinnedSHA256)
	}
	// 同一个待签串换个摘要函数：MD5 那一支也必须逐字对上。
	const pinnedMD5 = "f5334c48bc0cfb908e1623bb72ac1fdd"
	if got := keyAppendSignature(params, testCommSecret, signTypeMD5); got != pinnedMD5 {
		t.Fatalf("MD5 签名与钉死的向量不同：got %s, want %s", got, pinnedMD5)
	}

	// 换一把密钥、改一个值，签名都必须变——这正是「通讯密钥真的进了待签串」的证据。
	if keyAppendSignature(params, "another-secret", signTypeSHA256) == pinnedSHA256 {
		t.Fatal("换了密钥而签名没变：通讯密钥没进待签串")
	}
	tampered := testCallbackParams()
	tampered["totalAmount"] = "999900"
	if keyAppendSignature(tampered, testCommSecret, signTypeSHA256) == pinnedSHA256 {
		t.Fatal("改了金额而签名没变")
	}
}

// TestKeyAppendPayloadRules 把三条容易写错的规则单独钉一遍。
func TestKeyAppendPayloadRules(t *testing.T) {
	t.Run("签名参数与空值不参与", func(t *testing.T) {
		base := map[string]string{"b": "2", "a": "1"}
		withNoise := map[string]string{"b": "2", "a": "1", "empty": "", "sign": "xxx", "signature": "yyy"}
		if keyAppendPayload(base, "k") != keyAppendPayload(withNoise, "k") {
			t.Fatalf("空值或签名参数混进了待签串：%q", keyAppendPayload(withNoise, "k"))
		}
	})

	t.Run("按名字排序并直接接密钥", func(t *testing.T) {
		// totalAmount 排在 status 后面（字节序），末尾那段密钥**没有分隔符**。
		got := keyAppendPayload(map[string]string{"totalAmount": "1", "status": "S"}, "KEY")
		if want := "status=S&totalAmount=1KEY"; got != want {
			t.Fatalf("待签串 = %q, want %q", got, want)
		}
	})

	t.Run("大写字母排在所有小写字母前面", func(t *testing.T) {
		// 这是 ASCII 序与「字典序」的差别：`Z`(0x5A) 在 `a`(0x61) 之前。用 Go 的字符串比较
		// 天然就是这样，但值得钉一条——换成 sort.Slice 加一个 ToLower 的比较函数就会错。
		got := keyAppendPayload(map[string]string{"a": "1", "Z": "2"}, "")
		if want := "Z=2&a=1"; got != want {
			t.Fatalf("待签串 = %q, want %q", got, want)
		}
	})
}

// TestResolveSignType：默认 SHA256，两种拼法都认，认不出时**报错而不是静默回落**。
//
// 静默回落（比如认不出就用 SHA256）的后果是拿一个错的算法去验签、然后报「签名错误」——
// 而真正的原因是 signType 写的是我们没实现的算法。
func TestResolveSignType(t *testing.T) {
	cases := []struct {
		raw, want string
		wantErr   bool
	}{
		{"", signTypeSHA256, false},
		{"SHA256", signTypeSHA256, false},
		{"sha256", signTypeSHA256, false},
		{"SHA-256", signTypeSHA256, false},
		{" MD5 ", signTypeMD5, false},
		{"md5", signTypeMD5, false},
		{"RSA", "", true},
		{"SM3", "", true},
	}
	for _, tc := range cases {
		got, err := resolveSignType(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("resolveSignType(%q) 该报错，回了 %q", tc.raw, got)
			}
			// 错误串要点名那个取值：不然排查的人只知道「验签失败」。
			if !strings.Contains(err.Error(), tc.raw) {
				t.Fatalf("错误串里没提那个取值：%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("resolveSignType(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("resolveSignType(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestSignatureMatchesIgnoresHexCase：hex 的大小写不是信息。
//
// 渠道用它内部实现决定的那种写法（`%x` 是小写、Java 的 toUpperCase 是大写），而这一点没有
// 任何文档可以定稿。判错的代价是这一族一条回调都收不下，而折大小写对伪造者毫无帮助。
func TestSignatureMatchesIgnoresHexCase(t *testing.T) {
	const lower = "7f2cffe3296e96f87c4ff3804b844a290f1f1249588240e996cff577e12d3aca"
	if !signatureMatches(strings.ToUpper(lower), lower) {
		t.Fatal("大写 hex 该被认下来")
	}
	if !signatureMatches("  "+lower+"  ", lower) {
		t.Fatal("两侧空白该被 trim 掉")
	}
	if signatureMatches(lower[:len(lower)-1], lower) {
		t.Fatal("少一位的签名必须对不上")
	}
	if signatureMatches("", lower) {
		t.Fatal("空签名必须对不上")
	}
}

// TestBodyParamsReadsBothShapes：报文体按**内容**认，不按 Content-Type。
//
// 规范对支付结果通知写的是 form 表单，而老系统的回调入口按 Content-Type 分流成 JSON 或表单
// 两种（order_handler.go:1043）——说明真实投递里两种都出现过。只认表单的写法会在渠道改用
// JSON 的那一天把全部回调拒掉。
func TestBodyParamsReadsBothShapes(t *testing.T) {
	t.Run("表单", func(t *testing.T) {
		params, err := bodyParams([]byte("merOrderId=PAY-1&totalAmount=12800&rnd=a%2Bb"))
		if err != nil {
			t.Fatalf("bodyParams: %v", err)
		}
		if params["merOrderId"] != "PAY-1" {
			t.Fatalf("merOrderId = %q", params["merOrderId"])
		}
		// 值要**解码后**参与签名（规范：「值里有特殊字符要 URLEncode，但签名用原始值」）。
		if params["rnd"] != "a+b" {
			t.Fatalf("rnd = %q, want a+b（百分号编码的加号要解回加号）", params["rnd"])
		}
	})

	t.Run("JSON 对象", func(t *testing.T) {
		params, err := bodyParams([]byte(`{"merOrderId":"PAY-1","totalAmount":12800}`))
		if err != nil {
			t.Fatalf("bodyParams: %v", err)
		}
		// 数字要渲染成 `12800` 而不是 `1.28e+04`——后者进待签串就与渠道签的东西对不上。
		if params["totalAmount"] != "12800" {
			t.Fatalf("totalAmount = %q, want 12800", params["totalAmount"])
		}
	})

	t.Run("开头是 { 但不是 JSON 对象", func(t *testing.T) {
		if _, err := bodyParams([]byte(`{not json`)); err == nil {
			t.Fatal("该报错")
		}
	})
}

// TestVerifyAcceptsAFormCallbackSignedWithTheCommKey 是这一族 H5 那条路的正面。
func TestVerifyAcceptsAFormCallbackSignedWithTheCommKey(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")

	notification, err := New().Verify(context.Background(),
		keyAppendCallback(config, testCallbackParams(), "signature"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if notification.EventType != provider.EventSucceeded {
		t.Fatalf("EventType = %q", notification.EventType)
	}
	if notification.PaymentNo != "PAY20260917000000000001" {
		t.Fatalf("PaymentNo = %q", notification.PaymentNo)
	}
	// 表单那条路上每个值都是字符串，而 amountFen 同时吃两种形状——这条断言守的就是那件事。
	if notification.Amount != 12800 {
		t.Fatalf("Amount = %d, want 12800（分，表单里是字符串）", notification.Amount)
	}
	if got := notification.PaidAt.Format(requestTimestampLayout); got != "2026-09-17 10:20:30" {
		t.Fatalf("PaidAt = %q", got)
	}
}

// TestVerifyAcceptsAJSONCallbackSignedWithTheCommKey：同一条路，体是 JSON。
//
// 判据与上一条一模一样——**签名盖的是参数，不是字节**，所以两种投递方式走同一段归一化。
func TestVerifyAcceptsAJSONCallbackSignedWithTheCommKey(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	params := testCallbackParams()
	signature := legacyKeyAppendSignature(params, testCommSecret, signTypeSHA256)
	params["signature"] = signature

	// 报文里的字段与**签名时用的参数集**必须一模一样：少一个 payTime，签名就盖不住它，
	// 我们这边排出来的待签串也与渠道签的不同。
	body := []byte(fmt.Sprintf(
		`{"merOrderId":"%s","status":"%s","totalAmount":"%s","notifyId":"%s","payTime":"%s","signType":"%s","rnd":"%s","signature":"%s"}`,
		params["merOrderId"], params["status"], params["totalAmount"], params["notifyId"],
		params["payTime"], params["signType"], params["rnd"], signature))

	notification, err := New().Verify(context.Background(), provider.NotificationRequest{
		ChannelCode: "ums_wechat",
		Body:        body,
		Headers:     http.Header{},
		HTTPMethod:  http.MethodPost,
		Method:      channelMethod(config),
		Secrets:     commCredentials(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if notification.Amount != 12800 || notification.EventType != provider.EventSucceeded {
		t.Fatalf("JSON 投递的回调没被认下来：%+v", notification)
	}
}

// TestVerifyAcceptsTheSignatureParamUnderEitherName：`sign` 与 `signature` 都摘得掉。
//
// 规范对结果页明说是 `sign`，对支付结果通知一个名字都没给。两个都认。
func TestVerifyAcceptsTheSignatureParamUnderEitherName(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	for _, name := range []string{"sign", "signature", "Sign"} {
		t.Run(name, func(t *testing.T) {
			if _, err := New().Verify(context.Background(),
				keyAppendCallback(config, testCallbackParams(), name)); err != nil {
				t.Fatalf("签名参数叫 %q 时没认下来：%v", name, err)
			}
		})
	}
}

// TestVerifyRejectsAKeyAppendedCallbackThatChangedAnyParameter 是这一路最要紧的一组：
// **改了任何一个参与签名的参数，签名就必须对不上**。
func TestVerifyRejectsAKeyAppendedCallbackThatChangedAnyParameter(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")

	cases := []struct {
		name   string
		damage func(map[string]string)
	}{
		{"金额被改大", func(p map[string]string) { p["totalAmount"] = "999900" }},
		{"单号被换", func(p map[string]string) { p["merOrderId"] = "PAY-OTHER" }},
		{"随机字段被换", func(p map[string]string) { p["rnd"] = "another" }},
		{"多了一个没人签过的参数", func(p map[string]string) { p["extra"] = "x" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := testCallbackParams()
			// 先用**原始**参数签一份，再改参数——签名保持不变，报文变了。
			request := keyAppendCallback(config, params, "signature")
			tc.damage(params)
			request.Body = keyAppendFormBody(params, "signature", legacyKeyAppendSignature(testCallbackParams(), testCommSecret, signTypeSHA256))

			_, err := New().Verify(context.Background(), request)
			if !errors.Is(err, provider.ErrSignatureMismatch) {
				t.Fatalf("want ErrSignatureMismatch, got %v", err)
			}
			if errors.Is(err, provider.ErrInvalidNotification) {
				t.Fatalf("验签失败被报成了「签名过了、报文不合法」：%v", err)
			}
		})
	}
}

// TestVerifyRejectsAKeyAppendedCallbackSignedWithTheWrongKey：用 appKey 签入站报文必须对不上。
//
// 这一条守的是「两把密钥别搞混」：它们在这一族里方向相反，而**用错的那一把也会算出一个像模像样
// 的 hex 串**——判错的唯一表现就是验签失败。
func TestVerifyRejectsAKeyAppendedCallbackSignedWithTheWrongKey(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	params := testCallbackParams()
	request := keyAppendCallback(config, params, "signature")
	request.Body = keyAppendFormBody(params, "signature",
		legacyKeyAppendSignature(params, testSecret, signTypeSHA256))

	_, err := New().Verify(context.Background(), request)
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch, got %v", err)
	}
}

// TestVerifyNeverSkipsTheSignatureWhenTheCommKeyIsMissing：第二把凭据的 fail closed。
//
// 两条都要拒，而且**绝不能是 ErrInvalidNotification**（那一档会让调用方把
// signature_verified 记成 true）。槽名没配那条尤其要紧：凭据表里 `""` 这个键是渠道行
// secret_ref 的兜底路径，拿它去取值等于把 appKey 当成通讯密钥用。
func TestVerifyNeverSkipsTheSignatureWhenTheCommKeyIsMissing(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	params := testCallbackParams()

	t.Run("槽名没配", func(t *testing.T) {
		request := keyAppendCallback(channelConfig("https://api-mop.chinaums.com"), params, "signature")
		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSecretNotConfigured) {
			t.Fatalf("want ErrSecretNotConfigured, got %v", err)
		}
		if errors.Is(err, provider.ErrInvalidNotification) {
			t.Fatalf("验不了签被报成了「签名过了、报文不合法」：%v", err)
		}
		// 错误串要点名那一项配置：不然排查的人只会看到「密钥没配」而不知道配哪一把。
		if !strings.Contains(err.Error(), "sign.commSecretRef") {
			t.Fatalf("错误串里没点名配置项：%v", err)
		}
	})

	t.Run("槽是空的", func(t *testing.T) {
		request := keyAppendCallback(config, params, "signature")
		// appKey 在、通讯密钥不在——这一族的典型配置错误。
		request.Secrets = provider.Credentials{"appKey": testSecret}
		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSecretNotConfigured) {
			t.Fatalf("want ErrSecretNotConfigured, got %v", err)
		}
	})
}

// TestVerifyRejectsAKeyAppendedCallbackWithNoSignature：签名参数不在、头也没有。
//
// 这一条要报 ErrSignatureMismatch 而**不是** ErrSecretNotConfigured，而且它发生在读凭据
// **之前**：它说的是这条请求本身没有签名材料，与我们的槽配没配无关——把一份伪造的请求报成
// 「密钥没配」会让排查的人去翻配置，而真正该看的是「谁在打这个地址」。
func TestVerifyRejectsAKeyAppendedCallbackWithNoSignature(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	request := keyAppendCallback(config, testCallbackParams(), "signature")
	request.Body = keyAppendFormBody(testCallbackParams(), "signature", "")

	_, err := New().Verify(context.Background(), request)
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch, got %v", err)
	}
	if errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("一条没有签名材料的请求被报成了「密钥没配」：%v", err)
	}
}

// TestVerifyPrefersTheAuthorizationHeaderScheme：两套签名材料都在时，认头那一套。
//
// 判据是**形状**：带了 Authorization 头的请求声称自己是 OPEN-BODY-SIG，我们就按那一套验。
// 「两套都试一遍」的写法等于把两次验签里较松的那一次当成结论。
func TestVerifyPrefersTheAuthorizationHeaderScheme(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	params := testCallbackParams()
	body := keyAppendFormBody(params, "signature",
		legacyKeyAppendSignature(params, testCommSecret, signTypeSHA256))

	request := keyAppendCallback(config, params, "signature")
	request.Body = body
	// 头是别的形状：头那条路一定验不过，而 key 拼接那一套本来是能过的。
	request.Headers.Set(headerAuthorization, "Bearer xyz")

	_, err := New().Verify(context.Background(), request)
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("带了 Authorization 头却走了 key 拼接那条路：%v", err)
	}
}

// TestInboundDigestCarriesNoValues：诊断摘要里**只有参数名**，一个值都不含。
//
// 它会被写进 payment_notifications.failure_message 与日志，而回调报文里可能有用户的手机号
// 与卡号后四位。
func TestInboundDigestCarriesNoValues(t *testing.T) {
	params := map[string]string{
		"merOrderId":  "PAY20260917000000000001",
		"mobile":      "13800138000",
		"totalAmount": "12800",
		"signType":    signTypeSHA256,
	}
	digest := inboundDigest(params, false)

	for _, name := range []string{"merOrderId", "mobile", "totalAmount"} {
		if !strings.Contains(digest, name) {
			t.Fatalf("摘要里没有参数名 %q：%s", name, digest)
		}
	}
	for _, value := range []string{"13800138000", "12800", "PAY20260917000000000001"} {
		if strings.Contains(digest, value) {
			t.Fatalf("摘要里出现了参数值 %q：%s", value, digest)
		}
	}
	// signType 的**取值**是例外，它是必须记的：那正是「渠道用的是哪套摘要」这个待确认项。
	if !strings.Contains(digest, signTypeSHA256) {
		t.Fatalf("摘要里没有 signType：%s", digest)
	}
}

// TestVerifyReturnPage 是结果页回跳那条路。
//
// 它只验签、只带出**哪一单**，且事件类型是一个既不是 succeeded 也不是 failed 的值——这样
// 哪怕有人把回跳的路由接到了回调处理器上，provider.Notification 那条「其余取值记成 ignored
// 而不动支付状态」的既有约定会兜住。
func TestVerifyReturnPage(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")

	// returnRequest 造一次回跳。参数是**查询串**（GET 没有体），签名参数叫 sign（规范原文
	// §1.9.3：「其中 sign 是必返项」）。
	returnRequest := func(params map[string]string) provider.NotificationRequest {
		signature := legacyKeyAppendSignature(params, testCommSecret, params[paramSignType])
		query := url.Values{}
		for name, value := range params {
			query.Set(name, value)
		}
		query.Set("sign", signature)
		return provider.NotificationRequest{
			ChannelCode: "ums_wechat",
			Headers:     http.Header{},
			HTTPMethod:  http.MethodGet,
			RequestPath: "/v1/payments/return/ums_wechat",
			Query:       query,
			Method:      channelMethod(config),
			Secrets:     commCredentials(),
		}
	}

	t.Run("签名对", func(t *testing.T) {
		notification, err := New().Verify(context.Background(), returnRequest(map[string]string{
			"merOrderId": "PAY20260917000000000001",
			"status":     "TRADE_SUCCESS",
			"signType":   signTypeMD5,
		}))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if notification.EventType != eventReturn {
			t.Fatalf("EventType = %q, want %q", notification.EventType, eventReturn)
		}
		if notification.PaymentNo != "PAY20260917000000000001" {
			t.Fatalf("PaymentNo = %q", notification.PaymentNo)
		}
		// **不是**一个结算事件。这两个断言是这一条路存在的全部理由。
		if notification.EventType == provider.EventSucceeded || notification.EventType == provider.EventFailed {
			t.Fatal("回跳带上了一个结算事件类型：接错入口就会去动支付状态")
		}
		if notification.Amount != 0 {
			t.Fatalf("回跳不该带金额，实际 %d", notification.Amount)
		}
	})

	t.Run("不带 signType 时按 SHA256 算", func(t *testing.T) {
		if _, err := New().Verify(context.Background(), returnRequest(map[string]string{
			"merOrderId": "PAY-1",
		})); err != nil {
			t.Fatalf("缺 signType 该回落到 SHA256：%v", err)
		}
	})

	t.Run("改了参数", func(t *testing.T) {
		request := returnRequest(map[string]string{"merOrderId": "PAY-1", "status": "TRADE_SUCCESS"})
		request.Query.Set("merOrderId", "PAY-OTHER")

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSignatureMismatch) {
			t.Fatalf("want ErrSignatureMismatch, got %v", err)
		}
	})

	t.Run("没有签名", func(t *testing.T) {
		request := returnRequest(map[string]string{"merOrderId": "PAY-1"})
		request.Query.Del("sign")

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSignatureMismatch) {
			t.Fatalf("want ErrSignatureMismatch, got %v", err)
		}
	})

	t.Run("一个参数都没有", func(t *testing.T) {
		// 有人直接打了这个地址，而不是从收银台跳回来。
		request := returnRequest(map[string]string{"merOrderId": "PAY-1"})
		request.Query = url.Values{}

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSignatureMismatch) {
			t.Fatalf("want ErrSignatureMismatch, got %v", err)
		}
	})

	t.Run("没有通讯密钥", func(t *testing.T) {
		request := returnRequest(map[string]string{"merOrderId": "PAY-1"})
		request.Method = channelMethod(channelConfig("https://api-mop.chinaums.com"))

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSecretNotConfigured) {
			t.Fatalf("want ErrSecretNotConfigured, got %v", err)
		}
	})

	t.Run("签名过了但没说是哪一单", func(t *testing.T) {
		// 这一条**是** ErrInvalidNotification：签名确实验过了，只是报文缺了必要的那一项。
		_, err := New().Verify(context.Background(), returnRequest(map[string]string{
			"status": "TRADE_SUCCESS",
		}))
		if !errors.Is(err, provider.ErrInvalidNotification) {
			t.Fatalf("want ErrInvalidNotification, got %v", err)
		}
	})
}

// TestSecretSlotsReportTheSecondSlotOnlyWhenConfigured：槽名由配置说了算，配了才报。
//
// 没配说明这条渠道收不到 key 拼接签名的报文（只跑小程序那条路），那时报一个没有值的槽名只会
// 让装配处去找一把不存在的密钥。而 H5 的回调仍然 fail closed——判据在 commSecret 里。
func TestSecretSlotsReportTheSecondSlotOnlyWhenConfigured(t *testing.T) {
	method := channelMethod(commKeyConfig("https://api-mop.chinaums.com"))
	if got := New().SecretSlots(method); len(got) != 2 || got[0] != "appKey" || got[1] != "commKey" {
		t.Fatalf("SecretSlots = %q, want [appKey commKey]", got)
	}
}
