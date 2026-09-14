package client

import (
	"context"
	"log/slog"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// UnavailableSmsSender 是默认实现：不发短信，直接报错。
//
// 默认必须是它而不是日志实现。短信服务商在 V2 里还没有接，如果默认落到日志实现，
// 那么任何一个没配开关的环境都会把验证码明文写进日志——包括生产。默认失败，
// 再由 SMS_DEV_LOG_CODES 显式打开本地那条路，是唯一不会写错的方向。
type UnavailableSmsSender struct{}

func (UnavailableSmsSender) Send(context.Context, string, string) error {
	return service.ErrSmsUnavailable
}

// LogSmsSender 把验证码写进日志，只在本机开发用。
//
// 它存在的唯一理由是本地没有短信服务商，而验证码不发出去就没法登录——
// 本机不可能拿真手机号收短信。
type LogSmsSender struct{}

func (LogSmsSender) Send(ctx context.Context, phone, code string) error {
	// 手机号脱敏，验证码明文：这一行的用途就是让人读到验证码，脱敏了它就没用了；
	// 而手机号不需要明文也能对上号，少写一个明文就少一处泄露面。
	slog.WarnContext(ctx, "短信通道未配置，验证码仅写入日志", "phone", maskPhone(phone), "code", code)
	return nil
}

// maskPhone 保留前 3 位和后 2 位，中间打码。太短的号按位数全打码——那不是手机号，
// 按手机号的位次去切会把它截成一个更误导的值。
func maskPhone(phone string) string {
	// 按字节数判断是安全的：手机号是 ASCII 数字，不会出现「按字节切断字符」。
	if len(phone) < 7 {
		return strings.Repeat("*", len(phone))
	}
	return phone[:3] + strings.Repeat("*", len(phone)-5) + phone[len(phone)-2:]
}
