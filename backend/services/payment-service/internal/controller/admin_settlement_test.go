package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// 分账后台错误映射的用例。
//
// # 它守的是什么
//
// service 层那一批错误串是英文的（它们进日志），用户看到的是中文，翻这两边的就是
// settlementUserMessages 那张表 —— 三十来行的对照，逐行手写。这张用例要挡的正是「漏了一行」：
// 漏掉的后果不是报错，而是**一句英文原样弹在运营脸**上（settlementMessage 找不到就记一条 warn
// 再回兜底，请求照样 200 之外的状态码，页面上照样弹出来）。
//
// 所以它是**穷举**而不是抽样的：三个错误组各自走一遍真正的响应路径，逐条断言状态码 + 那句话
// 是表里的中文，而不是「返回了错误」这种要读代码才知道对不对的断言。
//
// 还有一件只有在这里能查的事：**表里没有一行是死的**。一行指向一个没人返回的错误值，它与
// 「漏了一行」在页面上同形（都是那句话不出现），而它比漏一行更难发现 —— 漏一行至少还有一条
// 测试红，多一行什么都不红。

// settlementErrorResponse 走一遍真正的响应路径，把状态码与整个响应体取回来。
//
// 走 writeAdminSettlementError 而不是断言某个中间变量：状态码与 errorCode 的对应关系就写在那个
// switch 里，绕过它测出来的东西与实际发给前端的可能不是同一件事。
func settlementErrorResponse(t *testing.T, err error) (int, api.Response) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/settlement/rules", nil)
	writeAdminSettlementError(recorder, request, err, "test fallback")

	var body api.Response
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("响应体不是合法的 JSON：%v", err)
	}
	return recorder.Code, body
}

// hasChinese 判断一句话里有没有汉字。
//
// 它是「英文串原样弹给运营」这条的唯一直接判据：那张表里的每一句都必须是中文，而一个从
// err.Error() 抄下来的答案会一字不差地通过「非空」与「不等于兜底」两条断言。
func hasChinese(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// assertUserFacing 是三个错误组共用的那三条断言：状态码、错误码、以及那句中文。
func assertUserFacing(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()

	status, body := settlementErrorResponse(t, err)
	if status != wantStatus {
		t.Fatalf("%v 回的是 %d，期望 %d", err, status, wantStatus)
	}
	if body.ErrorCode != wantCode {
		t.Fatalf("%v 的错误码是 %q，期望 %q", err, body.ErrorCode, wantCode)
	}
	if body.Success {
		t.Fatalf("%v 的响应里 success 是 true", err)
	}
	// 兜底那一句说明这张表里没有它 —— 也正是这条用例存在的理由。
	if body.ErrorMessage == msgGeneric {
		t.Fatalf("%v 在 settlementUserMessages 里没有对应的中文文案，用户看到的会是兜底", err)
	}
	if !hasChinese(body.ErrorMessage) {
		t.Fatalf("%v 弹给用户的是 %q，不是中文文案", err, body.ErrorMessage)
	}
}

// TestSettlementValidationErrorsAreFourHundred 逐条走一遍 service 的校验错误。
//
// 这一组是**用户在表单里填错**那一类：换个填法就能成功，所以是 400 而不是 409（分界写在
// settlementConflictErrors 的说明里）。IsValidationError 这个判据本身在 service 那边有单测，
// 这里验的是「它接上 HTTP 之后真的变成 400，而且真的有一句中文」。
func TestSettlementValidationErrorsAreFourHundred(t *testing.T) {
	if len(service.SettlementValidationErrors) == 0 {
		t.Fatal("service.SettlementValidationErrors 是空的：这条用例会变成一个空循环")
	}
	for _, err := range service.SettlementValidationErrors {
		t.Run(err.Error(), func(t *testing.T) {
			assertUserFacing(t, err, http.StatusBadRequest, "INVALID_ARGUMENT")
		})
	}
}

// TestSettlementConflictErrorsAreConflict 走一遍「库里的现状不接受它」那一组。
//
// 它们**不是**填错了哪一栏：同一个请求换一个时间点可能就成功了（那条同档位的规则被停用之后，
// 再配一条同档位的就通了）。所以是 409，而页面上的出口是「停用」，不是「改一改再提交」。
func TestSettlementConflictErrorsAreConflict(t *testing.T) {
	if len(settlementConflictErrors) == 0 {
		t.Fatal("settlementConflictErrors 是空的：这条用例会变成一个空循环")
	}
	for _, err := range settlementConflictErrors {
		t.Run(err.Error(), func(t *testing.T) {
			assertUserFacing(t, err, http.StatusConflict, api.CodeConflict)
		})
	}
}

// TestSettlementNotFoundErrorsAreNotFound 走一遍三个「查无此物」。
//
// **这条用例证明不了「仓储真的会产出这些错误值」**——它把哨兵直接喂给错误映射函数，整条
// 产生错误的路径（`GetSettlementTask` 扫到 pgx.ErrNoRows 那一刻）都在它外面。别把它当成
// 那个证据：这条断言加上「仓储确实翻了」才是全部，后者由
// repository.TestPostgresGetSettlementTaskMapsNoRows 真连库来守。
func TestSettlementNotFoundErrorsAreNotFound(t *testing.T) {
	if len(settlementNotFoundErrors) == 0 {
		t.Fatal("settlementNotFoundErrors 是空的：这条用例会变成一个空循环")
	}
	for _, err := range settlementNotFoundErrors {
		t.Run(err.Error(), func(t *testing.T) {
			assertUserFacing(t, err, http.StatusNotFound, api.CodeNotFound)
		})
	}
}

// TestSettlementWrappedErrorsKeepTheirMapping 确认包装过的错误照样认得出来。
//
// 仓储与服务这一路上到处都是 `fmt.Errorf("...: %w", err)`（那正是错误串进日志的方式），所以
// 这条映射链必须按 errors.Is 走而不是按相等走。少了这条用例，某天有人把 matchesAny 里的
// errors.Is 换成 == 之后，所有包装过的错误会一起掉进 500 —— 而 500 的那一句是「服务暂时不可用」，
// 页面上看不出任何差别。
func TestSettlementWrappedErrorsKeepTheirMapping(t *testing.T) {
	wrapped := fmt.Errorf("create settlement rule: %w", service.ErrSettlementRuleItemRatioOverflow)
	assertUserFacing(t, wrapped, http.StatusBadRequest, "INVALID_ARGUMENT")

	wrappedConflict := fmt.Errorf("delete settlement rule: %w", repository.ErrSettlementRuleInUse)
	assertUserFacing(t, wrappedConflict, http.StatusConflict, api.CodeConflict)

	// 包装的那个哨兵必须是**这条路由真会产生的**那一个。分账的读路径上产不出
	// ErrPaymentNotFound（支付单那两条路由各有各的表），拿它来当样本等于在验一条不存在的路。
	wrappedNotFound := fmt.Errorf("read settlement task: %w", repository.ErrSettlementTaskNotFound)
	assertUserFacing(t, wrappedNotFound, http.StatusNotFound, api.CodeNotFound)
}

// TestSettlementConstraintViolationIsFourHundred 单列那条兜底。
//
// 它**不在三个组里**，但它落在 400：走到这里的一定是「这份数据表不收」（service 逐列校验过、
// uuid 也 parse 过，能漏下来的只有列长度、check 之类的边角），对用户来说是「你填的不对」，
// 而不是「服务坏了再来一次」。表里给它留了一句话，所以它不该拿到服务不可用那句。
func TestSettlementConstraintViolationIsFourHundred(t *testing.T) {
	assertUserFacing(t, repository.ErrSettlementConstraintViolation,
		http.StatusBadRequest, "INVALID_ARGUMENT")
}

// TestSettlementUnknownErrorIsFiveHundred 确认没认出来的错误是 500 + 兜底，而不是 400。
//
// 反方向也要钉住：一个谁也没见过的错误（比如连接池断了）被当成「你填的不对」弹出去，会让用户
// 对着一张填得完全正确的表单反复改。真正的现场只在日志里，所以这里也确认它没把错误串塞进响应。
func TestSettlementUnknownErrorIsFiveHundred(t *testing.T) {
	status, body := settlementErrorResponse(t, errors.New("connection pool exhausted"))
	if status != http.StatusInternalServerError {
		t.Fatalf("未知错误回的是 %d，期望 500", status)
	}
	if body.ErrorCode != api.CodeInternal {
		t.Fatalf("未知错误的错误码是 %q，期望 %q", body.ErrorCode, api.CodeInternal)
	}
	if body.ErrorMessage != msgGeneric {
		t.Fatalf("未知错误弹给用户的是 %q，期望兜底那句", body.ErrorMessage)
	}
	if body.Success {
		t.Fatal("未知错误的响应里 success 不该是 true")
	}
}

// TestSettlementUserMessagesCoverEveryReachableError 是这张表的体检。
//
// 两件事：
//
//   - **没有重复行**。同一个错误值出现在两行里时，后一行永远不会被走到（settlementMessage 取第
//     一个命中的），而它看上去像是有文案的。
//   - **没有死行**。表里的每一行都要指向一个真的会被返回的错误值。指向一个没人返回的错误，
//     表现的正是这张表要防的那件事：那句话从来不会出现，而谁也没法从代码里一眼看出来。
func TestSettlementUserMessagesCoverEveryReachableError(t *testing.T) {
	reachable := make(map[error]bool)
	for _, group := range [][]error{
		service.SettlementValidationErrors,
		settlementConflictErrors,
		settlementNotFoundErrors,
		// 约束兜底那一支不算错误组（它按 errors.Is 单判），但它是能走到表里来的。
		{repository.ErrSettlementConstraintViolation},
	} {
		for _, err := range group {
			reachable[err] = true
		}
	}

	seen := make(map[error]bool, len(settlementUserMessages))
	for _, entry := range settlementUserMessages {
		if entry.err == nil {
			t.Fatal("settlementUserMessages 里有一行没有错误值")
		}
		if entry.text == "" {
			t.Fatalf("%v 的中文文案是空的", entry.err)
		}
		if entry.text == msgGeneric {
			t.Fatalf("%v 的中文文案就是兜底那一句，等于没写", entry.err)
		}
		if !hasChinese(entry.text) {
			t.Fatalf("%v 的文案 %q 不是中文", entry.err, entry.text)
		}
		if seen[entry.err] {
			t.Fatalf("%v 在 settlementUserMessages 里出现了两次：后一行永远不会被用到", entry.err)
		}
		seen[entry.err] = true

		if !reachable[entry.err] {
			t.Fatalf("%v 在表里有文案，但它不属于任何一个错误组——没有人会返回它", entry.err)
		}
	}

	// 反方向：每一个会被返回的错误值都得在表里有一行。这条与上面那三条断言里的「拿到的是
	// 兜底」是同一个结论，但它是**静态**的——不需要构造一条错误就知道表漏了谁。
	for err := range reachable {
		if !seen[err] {
			t.Fatalf("%v 会被返回给 controller，但 settlementUserMessages 里没有它的文案", err)
		}
	}
}
