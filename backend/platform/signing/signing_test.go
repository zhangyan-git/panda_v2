package signing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"maps"
	"math/big"
	"strings"
	"testing"
	"time"
)

// baselineParams / baselineSecret 是**基准用例的固定输入**。
//
// 期望值不是算出来的，是**跑出来的**：把 panda_serve 里的签名函数逐字复制到一个
// 独立程序里（见下），喂同一组输入打印结果。基准值就是那个结果。
//
//	fengxuan     internal/partners/fengxuan/client.go:408-438
//	shouchuang   internal/partners/shouchuang/client.go:52-82
//	             （与 directmealcard/client.go:93 逐字相同）
//	对外 API      internal/app/openapi/services/merchant_api_service.go:99-122
//	银联商务      internal/app/openapi/services/umspay_service.go:62-70
//	家园消费金    internal/partners/mallcashier/client.go:566-572
//
// 为什么值得这么绕：这几个算法的正确性**只能**以老系统为准 —— 它们是别人家的协议，
// 我们这边的"实现正确"就等于"和老系统签出来一模一样"。自己照着注释重写一遍再断言，
// 验的是自己的理解，不是协议。
//
// 复现命令（生成这些常量用的同一组输入）：
//
//	cd /tmp/panda-dev/sigbasis && go run .
var (
	baselineParams = map[string]string{
		"OrderNo":     "3CYM20260101120000001",
		"OrderAmount": "1280",
		"NotifyUrl":   "https://example.invalid/notify",
		"SubOpenId":   "oX-abc123DEF",
		"Remark":      "", // 空值：验证 SkipEmpty 生效
		"sign":        "", // 空值 + 在 Omit 名单里：验证两者都不参与
	}
	baselineSecret = "test-sign-key-9f3a"
)

func TestSignMatchesLegacyFengxuan(t *testing.T) {
	spec := Spec{
		Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"sign"}, KeyName: "key"},
		Algorithm: MD5Lower,
	}
	got, err := Sign(baselineParams, spec, Key{Secret: baselineSecret})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// panda_serve/internal/partners/fengxuan/client.go:408
	const want = "70edfb35901ce7bf120694cf8242f35e"
	if got != want {
		t.Errorf("fengxuan 签名与老系统不一致\n got %s\nwant %s", got, want)
	}
}

func TestSignMatchesLegacyShouchuang(t *testing.T) {
	spec := Spec{
		Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"Sign"}, KeyName: "Key"},
		Algorithm: MD5Upper,
	}
	got, err := Sign(baselineParams, spec, Key{Secret: baselineSecret})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// panda_serve/internal/partners/shouchuang/client.go:52（北方工业饭卡 directmealcard 逐字相同）
	const want = "A2068819F60A976B5DC3BB563FD2A600"
	if got != want {
		t.Errorf("shouchuang 签名与老系统不一致\n got %s\nwant %s", got, want)
	}
}

// TestOneProtocolFamilyThreeChannels 是本包存在的全部理由。
//
// 三份 spec 只差三处 —— Omit、KeyName、Algorithm —— 却跑出三家渠道各自的签名。
// 老系统为此写了两个 Go 包（fengxuan/client.go 与 shouchuang/client.go），
// 两份代码逐行相同、只差上面那三个值。这条断言把"三个渠道一个协议族"钉死：
// 哪天有人想再写一个业务渠道的包，先看看这条测试。
func TestOneProtocolFamilyThreeChannels(t *testing.T) {
	channels := []struct {
		name       string
		spec       Spec
		legacyWant string
	}{
		{
			name:       "丰选万联 / 优联",
			spec:       Spec{Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"sign"}, KeyName: "key"}, Algorithm: MD5Lower},
			legacyWant: "70edfb35901ce7bf120694cf8242f35e",
		},
		{
			name:       "首创饭卡 / 北方工业饭卡",
			spec:       Spec{Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"Sign"}, KeyName: "Key"}, Algorithm: MD5Upper},
			legacyWant: "A2068819F60A976B5DC3BB563FD2A600",
		},
	}

	for _, ch := range channels {
		t.Run(ch.name, func(t *testing.T) {
			got, err := Sign(baselineParams, ch.spec, Key{Secret: baselineSecret})
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if got != ch.legacyWant {
				t.Errorf("got %s, want %s", got, ch.legacyWant)
			}
		})
	}

	// 顺带钉死"只差一个字母"这件事本身：两份 spec 的待签串只应在 keyName 处不同。
	a := channels[0].spec.Canonical.Build(baselineParams, baselineSecret)
	b := channels[1].spec.Canonical.Build(baselineParams, baselineSecret)
	if strings.Replace(a, "&key=", "&Key=", 1) != b {
		t.Errorf("两家的待签串差异超出 keyName 一处\n丰选 %s\n首创 %s", a, b)
	}
}

func TestSignMatchesLegacyMerchantAPI(t *testing.T) {
	// 对外 API 的签名参数由 handler 拼进 map（merchant_api_handler.go:622）。
	params := map[string]string{
		"timestamp": "1767225600",
		"path":      "/api/v1/member/quota",
		"method":    "GET",
		"code":      "PANDA:xxx",
		"store_id":  "123",
	}
	// 与上两家的两点关键差异：**不过滤空值**，且密钥名是 secret、
	// 同时它又是 HMAC 的密钥（既拼进串又当 key）。
	spec := Spec{
		Canonical: Canonical{SkipEmpty: false, Sort: true, KeyName: "secret"},
		Algorithm: HMACSHA256Hex,
	}
	got, err := Sign(params, spec, Key{Secret: "abc123def456"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// panda_serve/internal/app/openapi/services/merchant_api_service.go:99
	const want = "607133fc2b739661ae681d70c41c5e16d34e2b9b9e573f537284e7aebdbce5fc"
	if got != want {
		t.Errorf("对外 API 签名与老系统不一致\n got %s\nwant %s", got, want)
	}
}

func TestSignPayloadMatchesLegacyUMS(t *testing.T) {
	body := []byte(`{"mid":"M001","tid":"T001","amount":1280}`)
	// 银联商务的待签串没有分隔符也没有排序：AppID + timestamp + nonce + sha256hex(body)。
	payload := "APPID001" + "20260101120000" + "1735700000000000000" + sha256Hex(body)

	got, err := SignPayload(payload, HMACSHA256Base64, Key{Secret: "UMSKEY-7788"})
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	// panda_serve/internal/app/openapi/services/umspay_service.go:62
	const want = "3dACMd0d76kMiyET7wbuSUAfoH7O6jbESBbUGTkyNy0="
	if got != want {
		t.Errorf("银联商务签名与老系统不一致\n got %s\nwant %s", got, want)
	}
}

func TestSignPayloadMatchesLegacyMallCashier(t *testing.T) {
	body := []byte(`{"orderNo":"3CYM20260101120000001","amount":1280}`)
	// 家园消费金：UPPER(method) \n path \n appKey \n timestamp \n nonce \n sha256hex(body)。
	// method 要转大写（"post" → "POST"），这一条由调用方负责 —— 它属于这家渠道的协议，
	// 不属于摘要算法。
	payload := strings.ToUpper("post") + "\n" + "/open/api/pay/create" + "\n" + "APPKEY-123" + "\n" +
		"1767225600" + "\n" + "abcdef0123456789" + "\n" + sha256Hex(body)

	got, err := SignPayload(payload, HMACSHA256Hex, Key{Secret: "SECRET-XYZ"})
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	// panda_serve/internal/partners/mallcashier/client.go:566
	const want = "982f020dd3b2e4c390cb6070c2c2cb0948ae91d46f676cf6f7afec0c15b7da2e"
	if got != want {
		t.Errorf("家园消费金签名与老系统不一致\n got %s\nwant %s", got, want)
	}
}

// —— 下面是与老系统行为**有意不同**的地方 ——

// TestVerifyUsesConstantTimeCompare 无法直接测时序，改为钉死可观察的行为契约：
// 长度不同的签名返回 ErrSignatureMismatch 而不是 panic 或别的错误。
//
// 老系统三处验签都是 `signature != expectedSignature`（merchant_api_service.go:92、
// fengxuan/client.go:396、directmealcard/client.go:325），普通字符串比较会在第一个
// 不同的字节处短路返回，攻击者能靠响应时间逐字节试出签名。
func TestVerifyRejectsWrongLengthSignature(t *testing.T) {
	spec := Spec{
		Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"sign"}, KeyName: "key"},
		Algorithm: MD5Lower,
	}
	key := Key{Secret: baselineSecret}

	for _, provided := range []string{"", "a", strings.Repeat("0", 31), strings.Repeat("0", 33)} {
		err := Verify(baselineParams, spec, key, provided)
		if !errors.Is(err, ErrSignatureMismatch) {
			t.Errorf("provided=%q: 期望 ErrSignatureMismatch，得到 %v", provided, err)
		}
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	spec := Spec{
		Canonical: Canonical{SkipEmpty: true, Sort: true, Omit: []string{"sign"}, KeyName: "key"},
		Algorithm: MD5Lower,
	}
	key := Key{Secret: baselineSecret}

	valid, err := Sign(baselineParams, spec, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(baselineParams, spec, key, valid); err != nil {
		t.Fatalf("原样验签应当通过，得到 %v", err)
	}

	// 改一个字节：金额从 1280 改成 1281。签名必须对不上。
	tampered := make(map[string]string, len(baselineParams))
	maps.Copy(tampered, baselineParams)
	tampered["OrderAmount"] = "1281"

	if err := Verify(tampered, spec, key, valid); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("改了金额后仍验签通过，得到 %v", err)
	}
}

func TestVerifyReportsMissingKeySeparately(t *testing.T) {
	// 「密钥没配」与「签名不对」必须是两个错误：前者是运维漏配（要告警），
	// 后者可能是有人在伪造（要拉黑）。混成一个会让排查往错误的方向走。
	spec := Spec{
		Canonical: Canonical{Sort: true, Omit: []string{"sign"}, KeyName: "key"},
		Algorithm: MD5Lower,
	}
	err := Verify(baselineParams, spec, Key{}, "whatever")
	if err == nil {
		t.Fatal("空密钥不应通过验签")
	}
	// MD5 类不要求 Key.Secret 非空（密钥是拼进串的，空密钥签出来的也是"合法"签名），
	// 所以这里是 ErrSignatureMismatch —— 关键是**不能放行**。
	if !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("期望 ErrSignatureMismatch，得到 %v", err)
	}

	// HMAC 类则明确报「没配密钥」。
	hmacSpec := Spec{Canonical: Canonical{Sort: true}, Algorithm: HMACSHA256Hex}
	if err := Verify(baselineParams, hmacSpec, Key{}, "x"); !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("HMAC 空密钥期望 ErrKeyNotConfigured，得到 %v", err)
	}
}

// —— Canonial 的行为 ——

func TestCanonicalOmitIsCaseSensitive(t *testing.T) {
	params := map[string]string{"a": "1", "sign": "old", "Sign": "old"}
	lower := Canonical{Sort: true, Omit: []string{"sign"}}
	upper := Canonical{Sort: true, Omit: []string{"Sign"}}

	// 两个渠道的字段名本来就差一个大小写，拿它当同一个键会把签名算错。
	if got := lower.Build(params, ""); got != "Sign=old&a=1" {
		t.Errorf("Omit=[sign] 得到 %q", got)
	}
	if got := upper.Build(params, ""); got != "a=1&sign=old" {
		t.Errorf("Omit=[Sign] 得到 %q", got)
	}
}

func TestCanonicalKeyIsAppendedEvenWhenParamsEmpty(t *testing.T) {
	// 无条件补 "&" 是与老系统逐字节一致的一部分（fengxuan/client.go:434）。
	// 参数为空是极端情况，但「看着别扭」不是改它的理由。
	got := Canonical{Sort: true, KeyName: "key"}.Build(map[string]string{}, "s3cr3t")
	if got != "&key=s3cr3t" {
		t.Errorf("得到 %q，期望 &key=s3cr3t", got)
	}
}

func TestCanonicalWithoutKeyNameAppendsNothing(t *testing.T) {
	got := Canonical{Sort: true}.Build(map[string]string{"b": "2", "a": "1"}, "ignored")
	if got != "a=1&b=2" {
		t.Errorf("得到 %q", got)
	}
}

func TestCanonicalSortMakesOutputStable(t *testing.T) {
	// 不排序时 Go 的 map 迭代顺序是随机的，同一份参数会签出不同的串。
	// 这条测试跑多次确认排序确实生效（不排序的实现会随机失败）。
	params := map[string]string{"z": "1", "a": "2", "m": "3", "b": "4"}
	canonical := Canonical{Sort: true}

	first := canonical.Build(params, "")
	for i := range 100 {
		if got := canonical.Build(params, ""); got != first {
			t.Fatalf("第 %d 次结果不同：%q vs %q", i, got, first)
		}
	}
}

// —— 算法注册表 ——

func TestParseAlgorithm(t *testing.T) {
	cases := []struct {
		in      string
		want    Algorithm
		wantErr bool
	}{
		{"md5_lower", MD5Lower, false},
		{"  MD5_UPPER  ", MD5Upper, false}, // 大小写与空白不敏感
		{"hmac_sha256_hex", HMACSHA256Hex, false},
		{"rsa2_sha256", RSA2SHA256, false},
		{"none", None, false},
		{"md5", "", true},
		{"", "", true},
		{"sha1", "", true},
	}
	for _, c := range cases {
		got, err := ParseAlgorithm(c.in)
		if c.wantErr {
			if !errors.Is(err, ErrAlgorithmUnknown) {
				t.Errorf("ParseAlgorithm(%q): 期望 ErrAlgorithmUnknown，得到 %v", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAlgorithm(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAlgorithm(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUnknownAlgorithmIsRejected(t *testing.T) {
	_, err := SignPayload("x", Algorithm("sha1"), Key{Secret: "k"})
	if !errors.Is(err, ErrAlgorithmUnknown) {
		t.Errorf("期望 ErrAlgorithmUnknown，得到 %v", err)
	}
}

func TestNoneAlgorithmProducesEmptySignature(t *testing.T) {
	// none 是给联调与内网渠道用的：它不签名，因此**空签名就能通过验签**。
	// 这条测试把这个危险的语义钉在明面上 —— 对外验签的路径上绝不能放它。
	sig, err := SignPayload("anything", None, Key{})
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	if sig != "" {
		t.Fatalf("none 应当产出空签名，得到 %q", sig)
	}
	if err := VerifyPayload("anything", None, Key{}, ""); err != nil {
		t.Errorf("空签名应当通过 none 的验签，得到 %v", err)
	}
	if err := VerifyPayload("anything", None, Key{}, "forged"); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("非空签名不该通过 none 的验签，得到 %v", err)
	}
}

// —— RSA2 ——

func TestRSA2RoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试密钥: %v", err)
	}
	key := Key{
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}),
		PublicKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustPKIX(t, &priv.PublicKey)}),
	}

	// 微信 APIv3 的待签串形状：METHOD\nURL\ntimestamp\nnonce\nbody\n（body 为空时末尾仍是 \n）。
	payload := "POST\n/v3/pay/transactions/jsapi\n1767225600\nnonce-abc\n{\"amount\":1}\n"

	sig, err := SignPayload(payload, RSA2SHA256, key)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	if err := VerifyPayload(payload, RSA2SHA256, key, sig); err != nil {
		t.Fatalf("原样验签应当通过，得到 %v", err)
	}

	// 改一个字节必须不过。
	tampered := strings.Replace(payload, "1767225600", "1767225601", 1)
	if err := VerifyPayload(tampered, RSA2SHA256, key, sig); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("改了时间戳后仍验签通过，得到 %v", err)
	}

	// 签名不是合法 base64 时按「签名不对」处理，不是配置错误。
	if err := VerifyPayload(payload, RSA2SHA256, key, "!!!not-base64!!!"); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("非法 base64 期望 ErrSignatureMismatch，得到 %v", err)
	}
}

func TestRSA2AcceptsPKCS8PrivateKey(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试密钥: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	key := Key{
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		PublicKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustPKIX(t, &priv.PublicKey)}),
	}

	// 支付宝密钥工具产出的是 PKCS#8，微信商户平台下发的是 PKCS#1，两种都得认。
	sig, err := SignPayload("x", RSA2SHA256, key)
	if err != nil {
		t.Fatalf("PKCS#8 私钥应当可用: %v", err)
	}
	if err := VerifyPayload("x", RSA2SHA256, key, sig); err != nil {
		t.Errorf("验签失败: %v", err)
	}
}

func TestRSA2VerifyAcceptsCertificate(t *testing.T) {
	// 微信 APIv3 的回调验签用的是**平台证书**，不是裸公钥。
	// parseRSAPublicKey 要能从 CERTIFICATE 块里取出公钥，适配器就不必自己 x509。
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试密钥: %v", err)
	}
	certPEM := selfSignedCertPEM(t, priv)

	signKey := Key{
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}),
	}
	sig, err := SignPayload("payload", RSA2SHA256, signKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}

	verifyKey := Key{PublicKeyPEM: certPEM}
	if err := VerifyPayload("payload", RSA2SHA256, verifyKey, sig); err != nil {
		t.Errorf("用证书验签失败: %v", err)
	}
}

func TestRSA2WithoutKeyReportsConfigurationProblem(t *testing.T) {
	_, err := SignPayload("x", RSA2SHA256, Key{})
	if !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("缺私钥时期望 ErrKeyNotConfigured，得到 %v", err)
	}
	// 错误里**不能带密钥内容** —— 虽然这里没有密钥，但这条不变量要一致地成立。
	if strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("错误信息疑似泄漏密钥内容: %v", err)
	}
}

// —— 辅助 ——

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustPKIX(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return der
}

// selfSignedCertPEM 造一张自签证书，只为让证书解析路径有东西可解。
// serial 固定，域名/有效期都无所谓 —— 这里不验证书，只验证"能从证书里取出公钥"。
func selfSignedCertPEM(t *testing.T, priv *rsa.PrivateKey) []byte {
	t.Helper()
	epoch := time.Unix(0, 0)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    epoch,
		NotAfter:     epoch.AddDate(10, 0, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
