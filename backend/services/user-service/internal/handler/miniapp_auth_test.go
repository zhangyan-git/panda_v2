package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// 每个错误对应客户端一个不同的动作，所以状态码必须各归各位：错误码合并成
// 500 就等于告诉客户端「随便吧」，它没法知道该重试、该改参数还是该去登录。
func TestWriteMiniappErrorMapsConsumerFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"不支持的登录方式", service.ErrLoginTypeUnsupported, http.StatusBadRequest},
		{"凭据缺项", service.ErrLoginCredentialMissing, http.StatusBadRequest},
		{"手机号格式不对", service.ErrInvalidPhone, http.StatusBadRequest},
		{"昵称不合法", service.ErrNicknameInvalid, http.StatusBadRequest},
		{"重发太快", service.ErrSmsSendTooFrequent, http.StatusTooManyRequests},
		{"验证码错误次数过多", service.ErrSmsCodeTooManyAttempts, http.StatusTooManyRequests},
		{"验证码错误", service.ErrSmsCodeInvalid, http.StatusUnauthorized},
		{"微信授权失败", service.ErrWechatAuthFailed, http.StatusUnauthorized},
		{"refresh 无效", service.ErrRefreshTokenInvalid, http.StatusUnauthorized},
		{"refresh 被复用", service.ErrRefreshTokenReused, http.StatusUnauthorized},
		{"账号已禁用", service.ErrConsumerDisabled, http.StatusForbidden},
		{"账号已注销", service.ErrConsumerDeleted, http.StatusForbidden},
		{"手机号被占用", model.ErrPhoneTaken, http.StatusConflict},
		{"微信被占用", model.ErrWechatIdentityTaken, http.StatusConflict},
		{"账号冲突", service.ErrAccountConflict, http.StatusConflict},
		{"微信不可用", service.ErrWechatUnavailable, http.StatusServiceUnavailable},
		{"短信不可用", service.ErrSmsUnavailable, http.StatusServiceUnavailable},
		// 令牌有效但用户行没了：客户端该做的是清掉本地令牌重登，与「没登录」同一处理。
		{"用户已不存在", fmt.Errorf("load user: %w", pgx.ErrNoRows), http.StatusUnauthorized},
		{"没见过的错误", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			writeMiniappError(res, tc.err, "操作失败")
			if res.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", res.Code, tc.wantCode, res.Body.String())
			}
		})
	}
}

// 冲突类错误是唯一「哨兵外面还裹着 pgconn」的一组：repository 为了保留 SQLSTATE
// 用了 %w 链。回显 err.Error() 会把表名、约束名、SQLSTATE 一起发给客户端——那既是
// 信息泄露，也是对客户端毫无用处的一串英文。这里把它钉住。
func TestWriteMiniappErrorDoesNotLeakDatabaseDetails(t *testing.T) {
	// 形状与 repository/user.go 的 userWriteError 一致，两处必须一起改。
	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "users_phone_key",
		Message:        `duplicate key value violates unique constraint "users_phone_key"`,
	}
	res := httptest.NewRecorder()
	writeMiniappError(res, fmt.Errorf("%w: %w", model.ErrPhoneTaken, pgErr), "绑定手机号失败")

	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusConflict)
	}
	body := res.Body.String()
	for _, leak := range []string{"SQLSTATE", "23505", "users_phone_key", "duplicate key"} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaked %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, model.ErrPhoneTaken.Error()) {
		t.Errorf("body should carry the sentinel text, got: %s", body)
	}
}

// 重发间隔要回给客户端，它才能显示「60 秒后再试」而不是让用户干等。
func TestWriteMiniappErrorSetsRetryAfterOnThrottle(t *testing.T) {
	res := httptest.NewRecorder()
	writeMiniappError(res, service.ErrSmsSendTooFrequent, "验证码发送失败")

	if got := res.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After is missing")
	}
}
