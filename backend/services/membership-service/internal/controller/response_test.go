package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// 这一组测试钉住的是同一件事：**回给用户的那句话是中文的，回到日志的那句话是英文的**。
//
// 写法与 payment-service / lottery-service 的那两份一样，理由也一样：admin-web 的
// requestErrorMessage 会优先用后端返回的 errorMessage，而服务层的错误串一律是英文（Go 的
// 惯例，grep 日志时好用）。少了这一层，运营在冻结会员失败时看到的就是一句
// "status is not one of active, frozen, expired, revoked"。
//
// 本域尤其不能漏：这一棵树上有一枚 membership:adjust，它动的是「这个人还能不能用会员价」，
// 每一次失败都对应一次有人坐不住的客服工单。

// errorGroups 是 controller 认得的全部错误分组，键是给测试失败时看的名字。
//
// 取自 writeMembershipError 用的那几个 var——**不是在这里另抄一份**：抄一份的话，新增一条
// 409 只改 controller 不改测试，测试照样绿，那它就只是个摆设。
func errorGroups() map[string][]error {
	return map[string][]error{
		"请求不合法": service.ValidationErrors,
		"找不到":   notFoundErrors,
		"状态冲突":  conflictErrors,
		"下游不可用": unavailableErrors,
		"身份缺失":  unauthorizedErrors,
	}
}

// TestEveryUserVisibleErrorHasAMessage 核对人话表是全集。
func TestEveryUserVisibleErrorHasAMessage(t *testing.T) {
	for name, group := range errorGroups() {
		if len(group) == 0 {
			t.Fatalf("「%s」这一组是空的：分组被改名或搬走了，这个测试就不再覆盖任何东西", name)
		}
		for _, err := range group {
			text, ok := userMessage(err)
			if !ok {
				t.Errorf("「%s」里的 %q 没有中文说法：userMessages 漏了一条", name, err)
				continue
			}
			if text == err.Error() {
				t.Errorf("%q 的「中文说法」与英文原文一字不差，那是照抄不是给用户看的话", err)
			}
		}
	}
}

// TestNoUserMessageIsDead 核对人话表里没有**够不着**的条目。
//
// 反过来的那一半：上一条测「每个分组里的错误都有话说」，这一条测「表里的每一句话都有人会收到」。
// 它挡的是一种很安静的腐烂——删掉一条校验、或把一条错误从分组里挪走之后，表里那一行还在，
// 读起来像是系统还会说那句话，实际上永远回不出来。写这一刀时已经手工删过一条
// （ErrAdjustExpireInvalid：那个字段在 dto 上就是 time.Time，格式错的请求根本走不到服务层）。
//
// 它同时把「分组必须覆盖表的全部」钉死：新增错误值又忘了加进分组时，这里会红——而不是等到
// 线上弹一句「操作失败，请稍后重试」（那正是漏分组的真实症状）。
func TestNoUserMessageIsDead(t *testing.T) {
	known := map[error]string{}
	for _, group := range errorGroups() {
		for _, err := range group {
			known[err] = ""
		}
	}

	for _, entry := range userMessages {
		if _, ok := known[entry.err]; !ok {
			t.Errorf("%q 有人话（%q），但它不在任何一个错误分组里：它永远走不到，"+
				"要么补进 writeMembershipError 的分组，要么把这一行删掉", entry.err, entry.text)
		}
	}
}

// TestEveryUserMessageIsChinese 逐条盯着整张表，而不只是分组里的那些。
//
// 与上面两条互补：它们关心「有没有」，这条关心「像不像人话」。
//
// 判据是**这句话里有中文字**，不是「一个拉丁字母都没有」：这些句子里合法地带着 ID、UUID
// 以及那几组枚举值（active / frozen / coupon…）。那些是**必须原样出现的**——运营要在后台的
// 筛选框里按这些值筛人，把它们译成中文反而对不上。真正的失败长这样：整句话一个汉字都没有，
// 那是一句照抄过来的英文原文。
func TestEveryUserMessageIsChinese(t *testing.T) {
	for _, entry := range userMessages {
		if !containsHan(entry.text) {
			t.Errorf("%q 的中文说法里一个汉字都没有，多半是照抄了英文原文：%q", entry.err, entry.text)
		}
	}
}

// containsHan 判断字符串里有没有汉字（CJK 统一表意文字基本区）。
//
// 手写而不是引一个包：这条规则只在这个测试里用一次，而它能表达的意思就这一句话。
func containsHan(value string) bool {
	for _, r := range value {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// TestAdjustErrorsSpeakSeparately 钉住后台调整有效期那几条**必须分开说的**错误。
//
// 它们是本域破坏力最大的那一次操作（没有任何订单、支付、流水跟着发生，只是把一个人的
// expire_at 挪一下），而它们全都长得像「状态不对」。合成一条的话，运营看到「这个会员不是
// 生效状态」，而他真正撞上的是「调整后的到期时间早于开通时间」——一个改一下日期就能过的错。
//
// 任意两条拿到同一句话，这个测试就红——它挡的正是「顺手合并成一句」。
func TestAdjustErrorsSpeakSeparately(t *testing.T) {
	seen := map[string]error{}
	for _, err := range []error{
		service.ErrMembershipRevoked,
		service.ErrMembershipNotActive,
		service.ErrMembershipNotFrozen,
		service.ErrExpireBeforeStart,
		service.ErrDuplicateChange,
		service.ErrAdjustExpireRequired,
	} {
		text, ok := userMessage(err)
		if !ok {
			t.Fatalf("%q 没有中文说法", err)
		}
		if other, dup := seen[text]; dup {
			t.Fatalf("%q 与 %q 用的是同一句话（%q）：用户分不清该做什么", err, other, text)
		}
		seen[text] = err
	}
}

// TestUserMessageFindsWrappedErrors 确认包装过的错误也认得出。
//
// 表用 errors.Is 逐个比而不是 map[error]string，理由就在这里：service 与 repository 会把
// 错误包一层上下文再往上返，包装之后值不再相等。
func TestUserMessageFindsWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("freeze membership: %w", service.ErrMembershipNotActive)
	text, ok := userMessage(wrapped)
	if !ok {
		t.Fatal("包装过的 ErrMembershipNotActive 没被认出来")
	}
	if text != "这个会员不是生效状态，冻结不了" {
		t.Fatalf("拿到的是 %q", text)
	}
}

// TestUnknownErrorFallsBackToTheGenericMessage 确认表外的错误不会退回英文。
//
// 兜底**故意不是 err.Error()**：那条路正是这张表要堵的。漏一条表的代价应该是「用户看到
// 一句不痛不痒的兜底话 + 日志里一条 warn」，而不是「用户看到一句英文」。
func TestUnknownErrorFallsBackToTheGenericMessage(t *testing.T) {
	text := userFacingMessage(context.Background(), fmt.Errorf("some brand new failure"))
	if text != genericErrorMessage {
		t.Fatalf("表外的错误回的是 %q，期望兜底话 %q", text, genericErrorMessage)
	}
	if strings.ContainsAny(text, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("兜底话里有拉丁字母：%q", text)
	}
}

// TestWriteMembershipErrorSpeaksChinese 走一遍真正的出口。
//
// 上面几条测的是表，这一条测的是**响应体**——两张表之间还隔着一次 switch 和一次 api.Error，
// 只有落到 body 上才算数。
func TestWriteMembershipErrorSpeaksChinese(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"套餐不存在", service.ErrPlanNotFound, http.StatusNotFound, "套餐不存在"},
		{"还不是会员", service.ErrMembershipNotFound, http.StatusNotFound, "这个人还不是会员"},
		{"编码被占", service.ErrPlanCodeTaken, http.StatusConflict, "这个套餐编码已经被占用了，换一个再存"},
		{"重复开通", service.ErrDuplicateChange, http.StatusConflict, "这一单的会员权益已经生效过了，没有重复开通"},
		{"要先签约", service.ErrAutoRenewNeedsSigning, http.StatusConflict, "自动续费要在小程序里签约开通，不能在这里打开"},
		{"价格填太大", service.ErrPlanPriceTooLarge, http.StatusBadRequest, "套餐价格填得太大了，请检查是不是多打了一位"},
		{"没写理由", service.ErrFreezeReasonRequired, http.StatusBadRequest, "请填写冻结原因"},
		// 包装过一层：service 与 repository 都是这么往上返的。
		{"包装过一层", fmt.Errorf("freeze membership: %w", service.ErrMembershipNotFrozen), http.StatusConflict, "这个会员没有被冻结，不用解冻"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/admin/memberships/x/freeze", nil)

			writeMembershipError(recorder, request, tc.err, "failed to freeze the membership")

			if recorder.Code != tc.status {
				t.Fatalf("状态码 %d，期望 %d", recorder.Code, tc.status)
			}
			var body struct {
				ErrorMessage string `json:"errorMessage"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应体不是 JSON：%v（%s）", err, recorder.Body.String())
			}
			if body.ErrorMessage != tc.text {
				t.Fatalf("errorMessage = %q，期望 %q", body.ErrorMessage, tc.text)
			}
			// 英文原文一个字都不该出现在响应体里。
			if strings.Contains(recorder.Body.String(), tc.err.Error()) {
				t.Fatalf("响应体里带着英文原文：%s", recorder.Body.String())
			}
		})
	}
}

// TestInternalErrorsDoNotLeakTheOperationName 确认 500 不把内部描述回给用户。
//
// `fallback` 是「这是哪一次请求」的英文描述（"failed to freeze the membership"），它的用途
// 是配合 slog 里的 error 定位问题，不进响应体。
func TestInternalErrorsDoNotLeakTheOperationName(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/memberships/x/freeze", nil)

	writeMembershipError(recorder, request, fmt.Errorf("dial tcp 127.0.0.1:5432: connect: refused"),
		"failed to freeze the membership")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 %d，期望 500", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "failed to freeze the membership") {
		t.Fatalf("500 把内部描述回了出去：%s", body)
	}
	if strings.Contains(body, "dial tcp") {
		t.Fatalf("500 把底层报错回了出去：%s", body)
	}
	if !strings.Contains(body, genericErrorMessage) {
		t.Fatalf("500 的响应体里没有兜底话：%s", body)
	}
}

// TestErrorGroupsAreDisjoint 四个分组两两不重叠。
//
// 这不是洁癖，是一条**会被静默吃掉**的约束：writeMembershipError 的 switch 里
// service.IsValidationError 排在最前，所以一个错误只要同时在 ValidationErrors 和别的组里，
// 它就永远走 400——后面那条 case 成了死代码，而写它的人以为自己在配 409。
//
// 本域离这条陷阱很近：ErrMembershipIDRequired / ErrMembershipIDInvalid 是 400，而
// ErrMembershipNotFound 是 404——三个名字长得像，把它们混在一组里是顺手的事。
// writeMembershipError 的注释里写着「两边的错误值不重叠，所以顺序本身不影响结果」——那句话在
// 这一条测试之前只是个说法。
func TestErrorGroupsAreDisjoint(t *testing.T) {
	groups := errorGroups()

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	// map 的遍历顺序是随机的，排一下序让失败信息稳定可读。
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for i, a := range names {
		for _, b := range names[i+1:] {
			for _, err := range groups[a] {
				for _, other := range groups[b] {
					if errors.Is(err, other) {
						t.Errorf("%q 同时在「%s」与「%s」里：先命中的那一组会赢，另一组是死代码",
							err, a, b)
					}
				}
			}
		}
	}
}
