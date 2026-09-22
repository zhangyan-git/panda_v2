// Package routes 把会员的 HTTP 路由挂到 kratos 的 router 上。
//
// 与其余几个服务同形：**鉴权由调用方套在最外层**，路由表只声明「哪个方法要哪个权限码」。
// 这样一处漏传校验器不可能变成失败开放——见 RegisterAdmin 里 authenticate 为 nil 的处理。
//
// 本服务是**两棵树**：后台（admin，按权限码鉴权）与小程序（miniapp，按用户令牌鉴权）。
// 小程序的会员中心与套餐列表读的是调用者自己的会员，路径上没有 user_id 参数——见
// miniapp.go 顶部那段「为什么没有 /users/{id}」。库存域只有一棵树，这一层是新增的。
package routes
