package controller

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这个文件是后台**只读**那一半的 HTTP 管道：解析请求、把错误写成响应。写那一半的解析与错误
// 对照表在 admin_config.go（它的错误有二十来条、要走一张表），这里只服务那三条 GET。
//
// **与 payment_response.go 是两套东西，不能合并**：那个文件服务的是渠道回调——它不 import
// platform/api、不用我们的错误信封（渠道不认），也不做身份判断（渠道带的是签名不是令牌）。
// 这个文件服务的是后台，一切按我们自己的协议来。两个文件放在同一个包里是因为它们都属于
// controller 这一层，但**它们的约定恰好是相反的**，所以各自的说明都写死在自己头上。
//
// 错误码用字面量 "INVALID_ARGUMENT" 而不是 api 的常量：platform/api 的常量表里没有它，
// 那张表是给跨服务的通用码（NOT_FOUND / CONFLICT / …）用的，请求参数的问题各服务一律
// 写字面量，与 membership-service / order-service / coupon-service 一致。

// 给用户看的中文句子。
//
// 这里**只有常量、没有表**：支付单这一族的只读路径上服务层只会产生一个错误值
// （repository.ErrPaymentNotFound），其余都是请求解析阶段就地挡下的，一句 switch 就写完了。
// 为一条错误值建一张表和一套「漏了会红」的测试，是在给一个不存在的问题做保险。
//
// 别的域不适用这条：分账那边**有**一张表（admin_settlement.go 的 settlementUserMessages），
// 因为它同时有读写两条路、错误值也多。判断依据是这个域有几种错误，不是「读还是写」。
//
// 写路径（admin_config.go）是另一回事：那边二十来条错误值、各有各的状态码，所以那一半用的是
// 表驱动 + 覆盖率测试。几处的做法不同，是因为它们要防的错不一样。
const (
	msgUnauthorized = "未登录"
	// msgPaymentNotFound：单号查无此单。**与「这单字段是空的」严格分开**——一张全是空
	// 字段的详情页看上去像一笔字段没落上的支付单，而真相是打开了一个不存在的单号。
	msgPaymentNotFound = "支付单不存在"
	// msgInvalidFilter：枚举类筛选（status / methodCode）传了词表以外的值。
	//
	// 不静默当成「不筛」：那样打错一个字母会返回**全部**支付单，而运营会把它当成「筛出来的
	// 结果」去对数。宁可 400。
	msgInvalidFilter = "筛选条件不合法"
	// msgInvalidUserID：userId 不是合法的 uuid。
	//
	// 单独一句话而不是并进 msgInvalidFilter，是因为这一条**不挡下来就是 500**：
	// payments.user_id 是 uuid 列，值原样进 SQL 时 PostgreSQL 会在解析参数时报错，
	// 那会以「服务出错」的面目出现在一个只是输错了的搜索框上。
	msgInvalidUserID = "用户 ID 不是合法的 UUID"
	// msgInvalidDate：时间筛选区间的某一端不是 RFC3339。用一句笼统的话而不是指出是哪一端，
	// 因为前端那两个 picker 传的是同一段代码格式化的值——错一定错在两端都在的地方。
	msgInvalidDate = "时间筛选格式不正确"
	// msgGeneric：兜底。用户看到这句话时对应的错误已经在日志里了。
	msgGeneric = "服务暂时不可用，请稍后重试"
)

// requireAdmin 取出后台身份。
//
// 与 membership-service 的同名函数逐字一致。这里的取值来自 auth.Middleware 放进请求上下文的
// 令牌声明——**不是自己解析令牌**：那会把「谁签的、有没有过期」这套判断抄第二遍。
//
// 走不到这条分支（authz 中间件已经在前面挡过一道）仍然是必须的：它让「handler 被单独装配
// 到别处」这种改动不会变成失败开放。
func requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)
		return "", false
	}
	return identity.UserID, true
}

// pageParams 读 page / pageSize，不合法时已经把响应写好了（第二个返回值为 false）。
//
// 摊掉的是每个 list handler 都要写的那六行（ParsePage → 判 ok → 拼一句话 → api.Error），
// 而它们每一处都得写对同一句话。句子由 platform/api.ParsePage 给——它知道是 page 还是
// pageSize 出的问题。
func pageParams(w http.ResponseWriter, r *http.Request, maxPageSize int) (page, pageSize int, ok bool) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), maxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return 0, 0, false
	}
	return page, pageSize, true
}

// parseOptionalTime 读一个可选的 RFC3339 时间。空串回 nil（= 没筛这一端）。
//
// 只认 RFC3339：前端用 dayjs 的 toRFC3339 生成的就是这个格式，而 parse 得松一点（认
// "2026-09-15" 之类的简写）会让「按天筛」与「按时刻筛」在后端看起来是同一个东西——
// 对账时那个差别正是「多出来一笔」的来源。
func parseOptionalTime(value string) (*time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, false
	}
	parsed = parsed.UTC()
	return &parsed, true
}

// timeRange 读一对区间端点，两端都没给时两个指针都是 nil。
//
// 两端分别解析、**不校验先后**：先后的校验属于业务规则，而本切片的两条查询（created_at
// 的上下界）用在 SQL 里时，start > end 只会返回空列表——它不是一个能被「修好」的错误，
// 只是没有这一行。为它加一条校验等于给一个不存在的问题写代码。
func timeRange(from, to string) (start, end *time.Time, ok bool) {
	start, ok = parseOptionalTime(from)
	if !ok {
		return nil, nil, false
	}
	end, ok = parseOptionalTime(to)
	if !ok {
		return nil, nil, false
	}
	return start, end, true
}

// parseOptionalUUID 读一个可选的 uuid。空串回 ""（= 没筛）。
//
// 在进 SQL 之前就挡下来，理由见 msgInvalidUserID：不是 uuid 的串会在 PostgreSQL 解析
// 参数时报错，然后以 500 的面目弹在搜索框上。
func parseOptionalUUID(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// restOf 取路径里前缀之后的那一段（去掉两端多余的斜杠）。
//
// 手写裁剪而不是取 gorilla/mux 的路由变量：路径模板写在 routes 里只是为了注册与指标里的
// 路径标签好看，真正被解析的是 r.URL.Path。与 membership-service 的同名函数是同一份实现。
func restOf(path, prefix string) string {
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

// writeAdminPaymentError 把服务层的错误翻成响应。
//
// 这一层是 HTTP 边界，也是唯一同时知道「这是哪一条错误」和「这句话是给谁看的」的地方。
// 服务层的错误串一律是英文（Go 的惯例，也让 grep 日志的语义保持清楚），而 admin-web 的
// requestErrorMessage 会**优先用后端返回的 errorMessage**——不翻译的话英文原样弹到运营脸上。
//
// 兜底那一支**必须记日志**：它对用户说「服务暂时不可用」，只有日志里才有真正的原因。
// 一个不记日志的兜底会让「支付页面打不开」变成一条没有任何线索的报障。
func writeAdminPaymentError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	// —— 找不到（404）——
	case errors.Is(err, repository.ErrPaymentNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, msgPaymentNotFound)

	default:
		slog.ErrorContext(r.Context(), "payment admin request failed", "error", err, "op", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, msgGeneric)
	}
}

// derefString 把可空字符串列转成 dto 里的普通字符串。
//
// 空串而不是指针：这两个列（legacy_id / secret_ref / remark）在 dto 里都是「有就显示、
// 没有就空着」，不存在「空值与非空值要分开处理」的场景——保持 dto 那一侧的类型简单。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
