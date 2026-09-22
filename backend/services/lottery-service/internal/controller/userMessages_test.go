package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// 这一组测试钉住的是同一件事：**回给用户的那句话是中文的，回到日志的那句话是英文的**。
//
// 起因是 2026-09-15 开通弹窗上弹出了「this location already has lottery enabled」——
// admin-web 的 requestErrorMessage 优先用后端返回的 errorMessage，而 controller 那时候
// 直接把 err.Error() 当成了响应体。修法是加了一张人话表；这一组测试保证那张表不会随着
// 新增错误值悄悄漏掉几条。

// TestEveryUserVisibleErrorHasAMessage 核对人话表是全集。
//
// 分组直接取自 writeLotteryError 用的那几个 var——**不是在这里另抄一份**：抄一份的话，
// 新增一条 409 只改 controller 不改测试，测试照样绿，那它就只是个摆设。
func TestEveryUserVisibleErrorHasAMessage(t *testing.T) {
	groups := map[string][]error{
		"请求不合法": service.ValidationErrors,
		"找不到":   notFoundErrors,
		"状态冲突":  conflictErrors,
		"身份缺失":  unauthorizedErrors,
	}

	for name, group := range groups {
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

// TestUserMessageFindsWrappedErrors 确认包装过的错误也认得出。
//
// 表用 errors.Is 逐个比而不是 map[error]string，理由就在这里：service 与 repository 会把
// 错误包一层上下文再往上返（`fmt.Errorf("activate: %w", err)`），包装之后值不再相等。
func TestUserMessageFindsWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("activate: %w", service.ErrLocationAlreadyActivated)
	text, ok := userMessage(wrapped)
	if !ok {
		t.Fatal("包装过的 ErrLocationAlreadyActivated 没被认出来")
	}
	if text != "这家门店已经开通了抽奖" {
		t.Fatalf("拿到的是 %q", text)
	}
}

// TestUnknownErrorFallsBackToTheGenericMessage 确认表外的错误不会退回英文。
//
// 兜底**故意不是 err.Error()**：那条路正是这一次要堵的。漏一条表的代价应该是「用户看到
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

// TestWriteLotteryErrorSpeaksChinese 走一遍真正的出口。
//
// 上面几条测的是表，这一条测的是**响应体**——两张表之间还隔着一次 switch 和一次
// api.Error，只有落到 body 上才算数。
func TestWriteLotteryErrorSpeaksChinese(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"已开通", service.ErrLocationAlreadyActivated, http.StatusConflict, "这家门店已经开通了抽奖"},
		{"门店不存在", service.ErrStoreNotFound, http.StatusNotFound, "这家门店在门店库里不存在，请刷新后重新选择"},
		{"参数不合法", service.ErrTargetNotPositive, http.StatusBadRequest, "参与门槛必须大于 0"},
		{"包装过一层", fmt.Errorf("activate: %w", service.ErrLocationAlreadyActivated), http.StatusConflict, "这家门店已经开通了抽奖"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/admin/lottery/activations", nil)

			writeLotteryError(recorder, request, tc.err, "failed to activate lottery for the location")

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
// `fallback` 是「这是哪一次请求」的英文描述（"failed to list lottery activations"），它的
// 用途是配合 slog 里的 error 定位问题。它曾经被原样写进响应体——那句话对用户没有任何意义，
// 而且和「不把内部报错弹给用户」这条规矩直接矛盾。
func TestInternalErrorsDoNotLeakTheOperationName(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/admin/lottery/activations", nil)

	writeLotteryError(recorder, request, fmt.Errorf("dial tcp 127.0.0.1:5432: connect: refused"),
		"failed to list lottery activations")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 %d，期望 500", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "failed to list lottery activations") {
		t.Fatalf("500 把内部描述回了出去：%s", body)
	}
	if strings.Contains(body, "dial tcp") {
		t.Fatalf("500 把底层报错回了出去：%s", body)
	}
	if !strings.Contains(body, genericErrorMessage) {
		t.Fatalf("500 的响应体里没有兜底话：%s", body)
	}
}
