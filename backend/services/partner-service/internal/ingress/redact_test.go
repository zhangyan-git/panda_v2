package ingress

import (
	"strings"
	"testing"
)

// 取货码那条路的报文里，值是必抹的——它是这台设备扣款的凭据，而调用日志是一张后台能读、
// 有自己保留期的表（见 redact.go 的文件头）。
func TestRedactSecretFieldsRemovesThePickupPassword(t *testing.T) {
	body := []byte(`{"provider":"linghang","deviceSerial":"SN-1","pickupPassword":"8421",` +
		`"drinkCode":"D-1","thirdPartyOrderNo":"TP-1"}`)

	got := string(redactSecretFields(body))

	if strings.Contains(got, "8421") {
		t.Fatalf("取货码留在报文里了：%s", got)
	}
	// 键要留着：「他没填这个字段」与「他填了但我们不记」是两句不同的话。
	if !strings.Contains(got, `"pickupPassword":"***"`) {
		t.Fatalf("抹掉之后应当只剩键与占位符，got %s", got)
	}
	// 其余字段一个字节都不该动。
	for _, want := range []string{`"provider":"linghang"`, `"deviceSerial":"SN-1"`, `"drinkCode":"D-1"`, `"thirdPartyOrderNo":"TP-1"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("报文里 %s 没了：%s", want, got)
		}
	}
}

// 值里带转义引号时，边界必须按 JSON 的规矩走：在 `\"` 处收尾会把密码的后半截留在日志里。
func TestRedactSecretFieldsHandlesEscapedQuotes(t *testing.T) {
	body := []byte(`{"pickupPassword":"a\"b\\c","deviceSerial":"SN-1"}`)

	got := string(redactSecretFields(body))

	if got != `{"pickupPassword":"***","deviceSerial":"SN-1"}` {
		t.Fatalf("got %s", got)
	}
}

// 没有这个字段时一个字节都不改：这个函数跑在验签之前，报文可能是任意形状的。
func TestRedactSecretFieldsLeavesOtherBodiesAlone(t *testing.T) {
	for _, body := range []string{
		`{"deviceSerial":"SN-1","amount":1500}`,
		`{"pickupPassword":null}`,
		`{"pickupPassword":1234}`,
		``,
		`not json at all`,
		// 断在字符串中间：宁可不抹，也不猜一个边界出来。
		`{"pickupPassword":"842`,
	} {
		if got := string(redactSecretFields([]byte(body))); got != body {
			t.Fatalf("body %q 被改成了 %q，它不该被改", body, got)
		}
	}
}

// 字段名出现在**某个值里面**时不是键，不该被抹（`:` 那一步挡掉的就是这种情况）。
func TestRedactSecretFieldsIgnoresTheNameInsideAValue(t *testing.T) {
	body := `{"remark":"pickupPassword 忘了填","deviceSerial":"SN-1"}`

	if got := string(redactSecretFields([]byte(body))); got != body {
		t.Fatalf("body %q 被改成了 %q，它不该被改", body, got)
	}
}
