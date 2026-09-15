package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Response 是所有接口的统一响应结构，符合 Ant Design Pro 规范。
// 前端通过 success 布尔值判断请求是否成功；错误场景使用
// errorCode（字符串常量）和 errorMessage（可读描述）。
type Response struct {
	Success      bool   `json:"success"`
	Data         any    `json:"data,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	// ShowType 控制前端错误展示方式：
	// 0=静默  1=消息条  2=通知  4=错误页
	ShowType int `json:"showType,omitempty"`
}

// PageResponse 是分页列表接口 data 字段的唯一形状，全部后台列表共用。
//
// items 是当前页数据，total 是满足筛选条件的总记录数（不是本页条数）。
// page/pageSize 原样回显请求值：前端据此确认服务端是按哪一页、多大页返回的，
// 出现「筛选后页码越界」这类情况时能看出来，而不是自己猜。
//
// 字段名是 camelCase，与其余后台字段一致；前端 ProTable 的 request 返回
// data.items / data.total 直接对应。
type PageResponse struct {
	Items    any   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
}

// ParsePage 解析 page/pageSize 查询参数，缺省为第 1 页、每页 20 条。
// ok=false 时 message 是可以直接回给调用方的错误描述（400 INVALID_ARGUMENT）。
//
// 放在这里而不是各 handler 各写一遍：列表接口的分页语义必须完全一致，
// 默认值或上限一旦分叉，前端就得按接口记不同的规矩。
func ParsePage(rawPage, rawSize string, maxPageSize int) (page, pageSize int, ok bool, message string) {
	page, pageSize = 1, DefaultPageSize
	if rawPage != "" {
		p, err := strconv.Atoi(rawPage)
		if err != nil || p < 1 {
			return 0, 0, false, "page must be a positive integer"
		}
		page = p
	}
	if rawSize != "" {
		s, err := strconv.Atoi(rawSize)
		if err != nil || s < 1 || s > maxPageSize {
			return 0, 0, false, "pageSize must be between 1 and " + strconv.Itoa(maxPageSize)
		}
		pageSize = s
	}
	return page, pageSize, true, ""
}

// PageOffset 把「第几页」换算成 SQL 的 OFFSET。
//
// 单独成函数是为了只有一个地方写这个减法：页码从 1 开始、偏移从 0 开始，
// 每处各写一遍 (page-1)*pageSize 迟早会有人漏掉那个 -1，而症状是第一页
// 凭空少一整页数据——不报错，只是看着像「刚建的记录不见了」。
func PageOffset(page, pageSize int) int {
	return (page - 1) * pageSize
}

const (
	// DefaultPageSize 是列表接口不传 pageSize 时的每页条数，与 Ant Design Pro
	// 表格默认每页 20 条对齐，免得前端首屏显示 10 条却告诉服务端拿了 20 条。
	DefaultPageSize = 20

	// MaxPageSize 是后台列表接口允许的最大 pageSize。
	//
	// 定在 200 而不是 20：后台有几个「要全集」的调用点（角色 Transfer、权限勾选、
	// 品牌/门店下拉），它们靠一次大页拿全量，而不是另开 options 接口。放宽上限的
	// 前提是这些集合本身远小于 200（实测权限 35 条、角色 2 条、菜单 17 条）。
	MaxPageSize = 200
)

// write 是内部写响应的基础函数。
func write(w http.ResponseWriter, httpStatus int, body Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	if httpStatus != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// Success 返回单个资源的成功响应（200）。
func Success(w http.ResponseWriter, data any) {
	write(w, http.StatusOK, Response{Success: true, Data: data})
}

// Created 返回资源创建成功响应（201）。
func Created(w http.ResponseWriter, data any) {
	write(w, http.StatusCreated, Response{Success: true, Data: data})
}

// Accepted 返回「收下了但还没做完」的成功响应（202）。
//
// 它与 Success 的区别不是状态码本身，而是 body 里那句「还不知道结果」：抽奖的参与要跨服务
// 扣卡，扣减超时的时候我们既不能说成功也不能说失败——用户的卡可能已经被扣了，也可能没有。
// 回 200 是在替一次没落地的操作背书，回 5xx 是在把一个未决说成故障（客户端会去重试，
// 而重试是由修复 worker 用同一个幂等号做的，不是客户端）。
func Accepted(w http.ResponseWriter, data any) {
	write(w, http.StatusAccepted, Response{Success: true, Data: data})
}

// NoContent 返回无内容成功响应（204），用于删除等操作。
func NoContent(w http.ResponseWriter) {
	write(w, http.StatusNoContent, Response{})
}

// Error 返回业务错误响应。
// code 使用 codes 包中定义的字符串常量，message 为面向用户的描述。
func Error(w http.ResponseWriter, httpStatus int, code string, message string) {
	write(w, httpStatus, Response{
		Success:      false,
		ErrorCode:    code,
		ErrorMessage: message,
	})
}

// ErrorWithShowType 同 Error，额外指定前端展示方式。
func ErrorWithShowType(w http.ResponseWriter, httpStatus int, code string, message string, showType int) {
	write(w, httpStatus, Response{
		Success:      false,
		ErrorCode:    code,
		ErrorMessage: message,
		ShowType:     showType,
	})
}
