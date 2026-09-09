package api

import (
	"encoding/json"
	"net/http"
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

// PageData 是列表接口 data 字段的标准结构。
// ProTable 的 request prop 默认读取 data.list 和 data.total。
type PageData struct {
	List  any   `json:"list"`
	Total int64 `json:"total"`
}

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

// NoContent 返回无内容成功响应（204），用于删除等操作。
func NoContent(w http.ResponseWriter) {
	write(w, http.StatusNoContent, Response{})
}

// SuccessPage 返回分页列表响应（200）。
// list 为当前页数据切片，total 为满足条件的总记录数。
func SuccessPage(w http.ResponseWriter, list any, total int64) {
	write(w, http.StatusOK, Response{
		Success: true,
		Data:    PageData{List: list, Total: total},
	})
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
