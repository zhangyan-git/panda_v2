package provider

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Config 是渠道 config 与渠道应答**共用**的读法，而这两件事对「空」的要求正好相反：
//
//   - 读 config 时，空值是「没填」，该回落到默认值；
//   - 读一份**应答**时，「键不在」与「键在但是空的」是两件不同的事（见 Has 的注释），
//     把它们混起来会让「对面可能已经建了单」变成「换个方式付」。
//
// 所以下面这一组用例的重点不在「能不能读出来」，而在**这两条边界上读到的是什么**。

func TestConfigReadsScalars(t *testing.T) {
	config := Config{
		"name":     "首创饭卡",
		"timeout":  30,  // Go 里构造的 config
		"ratio":    1.5, // JSON 解出来永远是 float64
		"enabled":  true,
		"disabled": false,
		"empty":    "",
		"nothing":  nil,
	}

	cases := []struct{ path, want string }{
		{path: "name", want: "首创饭卡"},
		// 数字与布尔按字符串读：JSON 里写 30 与写 "30" 在填的人看来是同一件事。
		{path: "timeout", want: "30"},
		// float64 走整数格式，%v 会打成 1.2e+06 这种形状（见 scalarString）。
		{path: "ratio", want: "1"},
		{path: "enabled", want: "true"},
		{path: "disabled", want: "false"},
		// 键在但是空的、键不在、键是 null：在这一层都是空串。
		{path: "empty", want: ""},
		{path: "nothing", want: ""},
		{path: "missing", want: ""},
	}
	for _, tc := range cases {
		if got := config.String(tc.path); got != tc.want {
			t.Fatalf("String(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}

	// 对象与数组在这两列里没有含义，读成空串。
	if got := config.String("endpoints"); got != "" {
		t.Fatalf("String 读一个对象该回空串，实际 %q", got)
	}
}

// TestConfigJSONNumbersReadTheSameAsGoNumbers 钉死「库里存的」与「Go 里写的」读到一样的值。
//
// 库里的 config 是 JSONB，解出来是 float64；测试与假渠道直接在 Go 里构造 map，那里是 int。
// 两边读法不一致的后果是一个配置项**在测试里生效、上了线悄悄回落到默认值**——
// `notify.ack.failStatus` 就这么踩过一次（400 而不是配的 200，且不报错）。
func TestConfigJSONNumbersReadTheSameAsGoNumbers(t *testing.T) {
	fromJSON := Config{}
	if err := json.Unmarshal([]byte(`{"failStatus":200,"skipEmpty":false}`), &fromJSON); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	fromGo := Config{"failStatus": 200, "skipEmpty": false}

	for _, path := range []string{"failStatus", "skipEmpty"} {
		if got, want := fromGo.String(path), fromJSON.String(path); got != want {
			t.Fatalf("String(%q)：Go 里写的读出 %q，库里存的读出 %q", path, got, want)
		}
	}
	if got := fromGo.Bool("skipEmpty", true); got {
		t.Fatal("skipEmpty 配了 false，Bool 却读成了 true")
	}
}

// TestHasSeparatesMissingFromEmpty 是 Has 存在的唯一理由。
func TestHasSeparatesMissingFromEmpty(t *testing.T) {
	config := Config{
		"status":     "",
		"falseValue": false,
		"zero":       0,
		"nested":     map[string]any{"status": ""},
		"nilValue":   nil,
	}

	present := []string{"status", "falseValue", "zero", "nested.status", "nested"}
	missing := []string{"absent", "nested.absent", "nested.status.deeper"}

	for _, path := range present {
		if !config.Has(path) {
			t.Fatalf("Has(%q) = false，但那个键在（值为空也算在）："+
				"「字段在、只是值为空」是渠道明确回了一个不是成功值的东西", path)
		}
	}
	for _, path := range missing {
		if config.Has(path) {
			t.Fatalf("Has(%q) = true，但那个键整个不在", path)
		}
	}

	// nil 也是「在」：JSON 的 null 与「键不在」在这一层不同——前者的字段确实出现了。
	if !config.Has("nilValue") {
		t.Fatal("值为 null 的键算「在」，Has 该回 true")
	}
}

func TestConfigObject(t *testing.T) {
	config := Config{
		"endpoints": map[string]any{"create": "https://channel.invalid/pay"},
		"sign":      "md5_lower", // 有人把一段声明写成了字符串
	}

	object, err := config.Object("endpoints")
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if object.String("create") != "https://channel.invalid/pay" {
		t.Fatalf("子对象读错了：%v", object)
	}

	// **键不存在返回空对象、不算错**：调用方多半会接着读里面的键，而那一层会给出
	// 「某某是必填的」这条更贴切的错。
	missing, err := config.Object("nothingHere")
	if err != nil {
		t.Fatalf("缺键不该报错（往下读会给出更贴切的错），实际 %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("缺键该回空对象，实际 %v", missing)
	}

	// 键在但不是对象才是这一层的错：往下读读不出东西了，早报比晚报好。
	if _, err := config.Object("sign"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("把字符串当对象读该报 ErrConfigInvalid，实际 %v", err)
	}
}

func TestConfigLookupReportsTheBrokenSegment(t *testing.T) {
	config := Config{"sign": "md5_lower"}

	_, err := config.RequireString("sign.algorithm")
	if err == nil {
		t.Fatal("路径中间遇到非对象该报错")
	}
	// 错误串要说清楚是**哪一段**读不下去，而不是只说末段的名字。
	if !strings.Contains(err.Error(), "config.sign") || !strings.Contains(err.Error(), "sign.algorithm") {
		t.Fatalf("错误串该同时给出坏掉的那一段与完整路径，实际 %v", err)
	}
}

func TestConfigRequireString(t *testing.T) {
	config := Config{"name": "  首创饭卡  ", "blank": "  "}

	value, err := config.RequireString("name")
	if err != nil {
		t.Fatalf("RequireString: %v", err)
	}
	if value != "首创饭卡" {
		t.Fatalf("值该被 trim 过，实际 %q", value)
	}

	for _, path := range []string{"blank", "absent"} {
		_, err := config.RequireString(path)
		if !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("RequireString(%q) 该报 ErrConfigInvalid，实际 %v", path, err)
		}
		// 错误里要带**完整路径**：配置是一棵树，只说「algorithm 没填」会让人去别的段里找。
		if !strings.Contains(err.Error(), "config."+path) {
			t.Fatalf("错误串该点名 config.%s，实际 %v", path, err)
		}
	}
}

func TestConfigStrings(t *testing.T) {
	config := Config{
		"request": map[string]any{"outTradeNo": "OrderNo", "amount": "OrderAmount", "seq": 1},
	}

	got, err := config.Strings("request")
	if err != nil {
		t.Fatalf("Strings: %v", err)
	}
	want := map[string]string{"outTradeNo": "OrderNo", "amount": "OrderAmount", "seq": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Strings = %v, want %v", got, want)
	}

	// 缺一段回空映射、不报错（调用方多半会接着读里面的键）。
	empty, err := config.Strings("absent")
	if err != nil || len(empty) != 0 {
		t.Fatalf("缺段该回空映射，实际 %v / %v", empty, err)
	}

	// **嵌套报错并点名是哪个键**：映射的键会变成渠道报文里的字段名，而一个嵌套对象
	// 做不了字段名。静默丢掉的话，渠道收到的报文会缺一项。
	nested := Config{"static": map[string]any{"app": map[string]any{"id": "1"}}}
	_, err = nested.Strings("static")
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("嵌套该报 ErrConfigInvalid，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "config.static.app") {
		t.Fatalf("错误串该点名坏掉的那个键，实际 %v", err)
	}

	// 数组同样不行：它拼不进待签串，也做不了字段名。
	array := Config{"static": map[string]any{"tags": []any{"a"}}}
	if _, err := array.Strings("static"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("数组该报 ErrConfigInvalid，实际 %v", err)
	}
}

// TestConfigFlatDropsNestedButKeepsEmpty 是验签那条路上最要紧的一条。
func TestConfigFlatDropsNestedButKeepsEmpty(t *testing.T) {
	config := Config{
		"order_no": "PAY1",
		"amount":   "12800",
		"subject":  "", // 空值必须留下
		"sign":     "", // 空值必须留下
		"count":    3,
		"data":     map[string]any{"status": "1"},
		"items":    []any{"a"},
	}

	got := config.Flat()
	want := map[string]string{
		"order_no": "PAY1",
		"amount":   "12800",
		"subject":  "",
		"sign":     "",
		"count":    "3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flat = %v, want %v", got, want)
	}

	// **空值保留是有意的**：待签串里要不要空值由签名规则的 SkipEmpty 决定，而那要按报文
	// 原样判断。在这里先滤掉的话，一个 SkipEmpty=false 的渠道会永远验不过签，而原因看上去
	// 是「对面算错了」。
	if _, present := got["subject"]; !present {
		t.Fatal("Flat 把空值滤掉了：SkipEmpty=false 的渠道会永远验不过签")
	}
	// **嵌套丢掉是有意的**：把 map[status:1] 这种 Go 字面量拼进待签串是一种永远对不上、
	// 但看起来像「算法错了」的静默错误。丢掉会让签名干脆对不上——那是个响亮的失败。
	if _, present := got["data"]; present {
		t.Fatal("Flat 把嵌套对象拼进了待签串")
	}
	if _, present := got["items"]; present {
		t.Fatal("Flat 把数组拼进了待签串")
	}
}

func TestConfigStringList(t *testing.T) {
	config := Config{
		"array":     []any{"2", "3"},
		"comma":     "2,3",
		"messy":     " 2 , , 3 ",
		"numbers":   []any{2, float64(3)},
		"withJunk":  []any{"2", map[string]any{"x": "1"}, "", " 3 "},
		"notAList":  "2",
		"isObject":  map[string]any{"a": "b"},
		"isNumber":  5,
		"absentKey": nil,
	}

	cases := []struct {
		path string
		want []string
	}{
		// 数组与逗号分隔的单串都认：这两种写法在运营眼里是同一件事，
		// 而「我写的是数组为什么报错」是个只浪费时间的错误。
		{path: "array", want: []string{"2", "3"}},
		{path: "comma", want: []string{"2", "3"}},
		{path: "messy", want: []string{"2", "3"}},
		{path: "numbers", want: []string{"2", "3"}},
		// 空项与嵌套项丢掉，其余 trim。
		{path: "withJunk", want: []string{"2", "3"}},
		{path: "isObject", want: nil},
		{path: "isNumber", want: nil},
		{path: "absentKey", want: nil},
		{path: "missing", want: nil},
	}
	for _, tc := range cases {
		if got := config.StringList(tc.path); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("StringList(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	// 单个取值的串就是只有一项的列表。
	if got := config.StringList("notAList"); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("StringList(notAList) = %v", got)
	}
}

func TestConfigBool(t *testing.T) {
	config := Config{
		"yes":     true,
		"no":      false,
		"quoted":  "true",
		"noisy":   " FALSE ",
		"garbage": "yes please",
		"number":  1,
	}

	cases := []struct {
		path string
		def  bool
		want bool
	}{
		{path: "yes", def: false, want: true},
		{path: "no", def: true, want: false},
		// 配置是手写的，有人在 JSON 里写 "true"。
		{path: "quoted", def: false, want: true},
		{path: "noisy", def: true, want: false},
		// 读不出布尔就回默认值，不报错：这一层没有错误可返回，而读不出值的那一栏
		// 会在别处以一条贴切得多的错误爆出来。
		{path: "garbage", def: true, want: true},
		{path: "garbage", def: false, want: false},
		{path: "number", def: true, want: true},
		{path: "missing", def: true, want: true},
		{path: "missing", def: false, want: false},
	}
	for _, tc := range cases {
		if got := config.Bool(tc.path, tc.def); got != tc.want {
			t.Fatalf("Bool(%q, %v) = %v, want %v", tc.path, tc.def, got, tc.want)
		}
	}
}
