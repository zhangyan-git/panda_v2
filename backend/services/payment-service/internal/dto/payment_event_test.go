package dto

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// orderServicePaymentEvent 是 order-service/internal/dto/payment.go 里 PaymentEventPayload
// 的**逐字镜像**。
//
// 为什么是手抄一份而不是 import：两个服务是两个 module，跨 module 不能互相 import；而这份
// 契约走的是 RabbitMQ 而不是 gRPC，contracts/ 里那条 proto 管不到它。对面解码用的是
// `DisallowUnknownFields`（order-service/internal/service/payment.go），所以**多发一个字段
// 就会让对方整条支付结果处理失败**——钱收了、订单不动。
//
// 手抄一份是这里唯一能做的守卫。抄错一个字，下面两个测试里至少有一个会红；而对面的结构
// 真改了，也只有人回来改这份镜像才会红——所以改动对面时**必须**顺手改这里，这条注释就是
// 给那个人看的。
type orderServicePaymentEvent struct {
	OrderNo               string                     `json:"orderNo"`
	PaymentNo             string                     `json:"paymentNo"`
	Amount                int64                      `json:"amount"`
	PaymentMethod         string                     `json:"paymentMethod"`
	Fundings              []orderServicePaymentEntry `json:"fundings"`
	ProviderTransactionID string                     `json:"providerTransactionId"`
	PaidAtUnix            int64                      `json:"paidAtUnix"`
	FailureCode           string                     `json:"failureCode"`
	FailureMessage        string                     `json:"failureMessage"`
}

// orderServicePaymentEntry 是对面 PaymentFunding 的镜像。
//
// 名字不叫 PaymentFunding：同一个包里重名就编译不过，而**名字不一样本身是对的**——它要
// 守住的是字段名与 json tag，不是 Go 类型名（对面换成什么名字与我们无关）。assertSameShape
// 因此只比字段名、json 名与类型，不比类型名。
type orderServicePaymentEntry struct {
	LineType       string  `json:"lineType"`
	Amount         int64   `json:"amount"`
	PaymentNo      string  `json:"paymentNo"`
	AccountEntryID *string `json:"accountEntryId"`
}

// TestPaymentEventPayloadShape 逐字段比对两边结构的形状。
//
// 比 DisallowUnknownFields 更严的地方在于它**两个方向都查**：多发一个字段、少发一个字段、
// 改一个 json tag、把 *string 换成 string，四条都会红。单靠解码只能查出「我们多发了」。
func TestPaymentEventPayloadShape(t *testing.T) {
	assertSameShape(t, reflect.TypeOf(PaymentEventPayload{}), reflect.TypeOf(orderServicePaymentEvent{}), "PaymentEventPayload")
}

// TestPaymentEventPayloadDecodesOnTheOtherSide 用**对面真实的解码方式**解一遍我们真的编
// 出来的那份 JSON。
//
// 形状比对是静态的，这个测试是动态的：它覆盖形状比对看不见的那一类漂移——比如某个字段被
// 加上 `json:"-"`（形状还是对的，但值永远不出去），或者某个 tag 里多了个不在断言范围内的
// 选项。
func TestPaymentEventPayloadDecodesOnTheOtherSide(t *testing.T) {
	entryID := "entry-1"
	original := PaymentEventPayload{
		OrderNo:               "ORD20260914000001",
		PaymentNo:             "PAY20260914120000000001",
		Amount:                1980,
		PaymentMethod:         "wechat",
		Fundings:              []PaymentFunding{{LineType: "channel", Amount: 1980, PaymentNo: "PAY20260914120000000001", AccountEntryID: &entryID}},
		ProviderTransactionID: "MANUAL-PAY20260914120000000001",
		PaidAtUnix:            1789000000,
		FailureCode:           "",
		FailureMessage:        "",
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal payment event payload: %v", err)
	}

	var decoded orderServicePaymentEvent
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	// 与对面一致。**这一行是这个测试的全部意义**：不加它，多发的字段会被静默丢掉，
	// 测试永远是绿的，而线上对面会整条解不出来。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("order-service cannot decode our payload: %v\npayload: %s", err, encoded)
	}

	if decoded.OrderNo != original.OrderNo ||
		decoded.PaymentNo != original.PaymentNo ||
		decoded.Amount != original.Amount ||
		decoded.PaymentMethod != original.PaymentMethod ||
		decoded.ProviderTransactionID != original.ProviderTransactionID ||
		decoded.PaidAtUnix != original.PaidAtUnix ||
		decoded.FailureCode != original.FailureCode ||
		decoded.FailureMessage != original.FailureMessage {
		t.Fatalf("scalar fields did not survive the round trip: got %+v, sent %+v", decoded, original)
	}
	if len(decoded.Fundings) != 1 {
		t.Fatalf("fundings: got %d entries, want 1", len(decoded.Fundings))
	}
	funding := decoded.Fundings[0]
	if funding.LineType != "channel" || funding.Amount != 1980 || funding.PaymentNo != original.PaymentNo {
		t.Errorf("funding fields did not survive the round trip: got %+v", funding)
	}
	if funding.AccountEntryID == nil || *funding.AccountEntryID != entryID {
		// 指针字段要单独断言：断言成 `!= ""` 会让「字段整个丢了」和「发了个空串」看起来
		// 一样，而两者对面看到的是不同的东西（nil vs 一个给了但为空的 id）。
		t.Errorf("accountEntryId did not survive the round trip: got %v, want %q", funding.AccountEntryID, entryID)
	}
}

// TestPaymentEventRoutingKeys 把两个路由键与信封版本钉成字面量。
//
// 它们是 RabbitMQ 的 routing key（交易所按它路由），不是普通的常量：改一个字母，事件就会
// 投进一个没人绑定的路由然后被**静默丢掉**——Publish 照样报成功（见
// [[publish-treats-unroutable-as-success]]）。字符串本身还有另一处硬引用：order-service
// 的 dto.EventPaymentSucceeded / dto.EventPaymentFailed。所以它值得一个自己的测试，
// 而不是靠上面那两份结构比对顺带覆盖。
func TestPaymentEventRoutingKeys(t *testing.T) {
	if EventPaymentSucceeded != "payment.succeeded" {
		t.Errorf("EventPaymentSucceeded = %q, want %q", EventPaymentSucceeded, "payment.succeeded")
	}
	if EventPaymentFailed != "payment.failed" {
		t.Errorf("EventPaymentFailed = %q, want %q", EventPaymentFailed, "payment.failed")
	}
	if EventVersion != "v1" {
		t.Errorf("EventVersion = %q, want %q", EventVersion, "v1")
	}
}

// assertSameShape 比对两个结构体的字段数、字段名、json 名与类型。
//
// 不比 Go 类型名：镜像的类型名与对面不同是有意的（见 orderServicePaymentEntry 的注释）。
func assertSameShape(t *testing.T, ours, theirs reflect.Type, path string) {
	t.Helper()
	if ours.Kind() != reflect.Struct || theirs.Kind() != reflect.Struct {
		t.Fatalf("%s: both sides must be structs, got %s / %s", path, ours.Kind(), theirs.Kind())
	}
	if ours.NumField() != theirs.NumField() {
		t.Fatalf("%s: field count differs, ours %d, theirs %d — 少发一个字段对面拿到的是一份缺数据的订单",
			path, ours.NumField(), theirs.NumField())
	}
	for i := 0; i < ours.NumField(); i++ {
		mine, other := ours.Field(i), theirs.Field(i)
		child := path + "." + mine.Name
		if got, want := jsonName(mine), jsonName(other); got != want {
			t.Errorf("%s: json name differs, ours %q, theirs %q", child, got, want)
		}
		if mine.Name != other.Name {
			t.Errorf("%s: Go field name differs, ours %q, theirs %q", child, mine.Name, other.Name)
		}
		assertSameType(t, mine.Type, other.Type, child)
	}
}

// assertSameType 比类型，并在两边都是「本包里的命名结构体」时递归。
//
// 递归的边界放在包路径上而不是无条件递归：time.Time 这类标准库类型两边是**同一个**类型，
// 逐字段比只会比到它的未导出字段上，那不是契约。本包的结构体则必须逐字段比——对面把
// *string 改成 string 时，只比最外层类型发现不了。
func assertSameType(t *testing.T, ours, theirs reflect.Type, path string) {
	t.Helper()
	switch ours.Kind() {
	case reflect.Slice, reflect.Ptr:
		assertSameType(t, ours.Elem(), theirs.Elem(), path+"[]")
	case reflect.Struct:
		if ours.PkgPath() == theirs.PkgPath() {
			assertSameShape(t, ours, theirs, path)
			return
		}
		fallthrough
	default:
		if ours.String() != theirs.String() {
			t.Errorf("%s: type differs, ours %s, theirs %s", path, ours, theirs)
		}
	}
}

// jsonName 取字段的 json 名，忽略 omitempty 那类选项。
//
// 没有 tag 时返回 Go 字段名：encoding/json 就是这么干的，返回空串会让「两边都没写 tag」
// 被报成一个错误。
func jsonName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}
