// Package routes 把福卡账户的 HTTP 路由挂到平台的 HTTPRouter 上。
//
// 认证/授权由调用方（cmd/main.go）注入，不在这里构造：一个忘记传认证的路由必须变成
// 明确拒绝，而不是敞开——见 RegisterAdmin 里的 unauthorized 兜底。
package routes
