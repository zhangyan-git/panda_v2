package controller

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

// 这个文件是 HTTP 边界的公共工具：取身份、读分页、读时间与 uuid、裁路径、解请求体。
//
// 它是**两棵树共用的**（后台树与开放接口树，见 doc.go）：这些函数里没有一条与「谁是调用方」
// 有关——它们只把 r 变成一个值或一句错误。而两棵树的**答复**不同（中文/英文、文案不同），
// 所以这里一个响应都不写：凡是要写响应的函数都留在各自的树里（writeAdminPartnerError /
// writeOpenAPIError）。
//
// 错误码用字面量 "INVALID_ARGUMENT" 而不是 api 的常量：platform/api 的常量表里没有它，
// 那张表是给跨服务的通用码（NOT_FOUND / CONFLICT / …）用的，请求参数的问题各服务一律写字面量，
// 与 membership-service / order-service / coupon-service / payment-service 一致。

// 给用户看的中文句子（后台树）。
//
// 这里只放**两棵树都会用到的那几个**，剩下的在 admin_partner.go 的错误对照表里。判断标准很
// 简单：写在这个文件里的句子对应的是「请求本身不成立」（没登录、body 读不出来、路径里的 id
// 不是 uuid），它们在任何一条后台路径上都一样；对照表里的那些对应的是**某个具体的业务对象**，
// 换个模块就要换一句话。
const (
	msgUnauthorized = "未登录"
	// msgGeneric：兜底。用户看到这句话时对应的错误已经在日志里了。
	msgGeneric = "服务暂时不可用，请稍后重试"
	// msgEmptyBody：请求体是空的。与 msgInvalidBody 分开：一个是没发、一个是发错了。
	msgEmptyBody = "请求体不能为空"
	// msgInvalidBody：请求体不是合法 JSON，或者带了不认识的字段（多半是字段名拼错了）。
	msgInvalidBody = "请求体格式不正确"
	// msgInvalidPathID：路径里的 id 不是 uuid。
	//
	// 在进 SQL 之前就挡下来，理由与 payment 的 msgInvalidUserID 逐字相同：不是 uuid 的串会让
	// PostgreSQL 解析参数时报 22P02，于是「复制粘贴少了一截的 id」会以「服务出错」的面目出现。
	msgInvalidPathID = "id 不是合法的 UUID"
	// msgInvalidFilter：枚举类筛选传了词表以外的值。
	//
	// 不静默当成「不筛」：那样打错一个字母会返回**全部**记录，而运营会把它当成「筛出来的结果」
	// 去对数。宁可 400。
	msgInvalidFilter = "筛选条件不合法"
	// msgInvalidDate：时间筛选区间的某一端不是 RFC3339。用一句笼统的话而不是指出是哪一端，
	// 因为前端那两个 picker 传的是同一段代码格式化的值——错一定错在两端都在的地方。
	msgInvalidDate = "时间筛选格式不正确"
)

// requireAdmin 取出后台身份。
//
// 与 payment-service / membership-service 的同名函数逐字一致。取值来自 auth.Middleware 放进
// 请求上下文的令牌声明——**不是自己解析令牌**：那会把「谁签的、有没有过期」这套判断抄第二遍。
//
// 走不到这条分支（authz 中间件已经在前面挡过一道）仍然是必须的：它让「handler 被单独装配到
// 别处」这种改动不会变成失败开放。
func requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)
		return "", false
	}
	return identity.UserID, true
}

// pageParams 读 page / pageSize，不合法时已经把响应写好了（第三个返回值为 false）。
//
// 上限传 api.MaxPageSize（200），本服务不另立一个：后台所有列表的分页语义必须完全一致。
// payment-service 那边写的是 dto.MaxPageSize，是它自己的 dto 转出来的同一个常量——本服务的
// dto 里刻意没有转，多一处「默认值」就多一处「这一页怎么只显示 N 条」的解释。
func pageParams(w http.ResponseWriter, r *http.Request) (page, pageSize int, ok bool) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return 0, 0, false
	}
	return page, pageSize, true
}

// parseOptionalTime 读一个可选的 RFC3339 时间。空串回 nil（= 没筛这一端）。
//
// 只认 RFC3339：前端用 dayjs 的 toRFC3339 生成的就是这个格式，而 parse 得松一点（认
// "2026-09-15" 之类的简写）会让「按天筛」与「按时刻筛」在后端看起来是同一个东西——调用日志
// 按天排查时，那个差别正是「多出来一次调用」的来源。
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
// 两端分别解析、**不校验先后**：start > end 在 SQL 里只会返回空列表，它不是一个能被「修好」
// 的错误，只是这一段时间里没有记录。为它加一条校验等于给一个不存在的问题写代码。
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
// 在进 SQL 之前就挡下来，理由同 msgInvalidPathID：不是 uuid 的串会在 PostgreSQL 解析参数时
// 报错，然后以 500 的面目弹在一个只是输错了的筛选框上。
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

// parseOptionalInt 读一个可选的正整数筛选值（调用日志的 statusCode）。
//
// **只判形状，不判取值范围**：100..599 那一条属于业务规则，在 service 里（它要返回一个能被
// 错误对照表翻成中文的哨兵错误）。这里判的是「这串东西是不是一个数」——`statusCode=abc`
// 进 SQL 会被 pgx 在参数编码时报错，那是一个 500。
func parseOptionalInt(value string) (int, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, true
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// restOf 取路径里前缀之后的那一段（去掉两端多余的斜杠）。
//
// 手写裁剪而不是取路由变量：路径模板写在 routes 里只是为了注册与指标里的路径标签好看，真正
// 被解析的是 r.URL.Path。与 membership-service / payment-service 的同名函数是同一份实现。
func restOf(path, prefix string) string {
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

// decodeJSON 把请求体读进 v。
//
// DisallowUnknownFields 是有意的：字段名拼错时静默忽略，会变成一个「保存成功了但那一项没变」
// 的工单，而那种工单查起来最费劲——页面上显示的是你填的值，库里是旧的。
//
// 给用户看的是一句中文，**原始错误进日志**：Go 的解析错误里带着字段名
// （`json: unknown field "ipWhitelst"`），那句英文对排查有用、对运营没用。
//
// 限 1 MiB：本服务的请求体最大的一条也只是十几个字段的表单，给它一个上限是为了「一个畸形的
// 大 body 不会把内存吃光」。开放接口那边有一条更大的上限（见 ingress.maxLoggedBodyBytes），
// 但那是**记录**用的，不是解析用的。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgEmptyBody)
			return false
		}
		slog.WarnContext(r.Context(), "partner admin body rejected",
			"error", err, "path", r.URL.Path)
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return false
	}
	return true
}

// deref 把可空列转成 dto 里的空串。
//
// 只用在**确实没有「空值与非空值要分开处理」**的那几列上（备注、描述、审计里的 reason 之类）；
// expiresAt 那种 null 有含义的列一律原样透传指针，让前端那个 `—` 有东西可依。
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
