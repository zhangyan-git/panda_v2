// Package controller 是福卡账户的 HTTP 适配层：解析请求、调用 service、把错误翻成状态码。
//
// 业务规则不在这里——它属于 service 层（见那一包的说明）。这里唯一值得单独说的东西是
// 路由分发：路径按段数分支，注册顺序见 routes 包。
package controller
