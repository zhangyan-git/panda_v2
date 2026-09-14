// Package routes 把 controller 挂到 HTTP 路由上，并按端各自注册一棵路径树。
//
// 三条不能破坏的约定：
//
//   - 认证由调用方注入，不由这里new一个。装配处（cmd/main.go）是唯一知道「哪个端该套
//     哪条中间件链」的地方；本包只保证「没给中间件就回 401」，绝不 fail-open。
//   - 路径注册顺序是从长到短。底层是 gorilla/mux，按注册顺序取第一个匹配，
//     把 /{id}/cancel 写在 /{id} 之后会让它永远解析成 id="cancel"。
//   - 权限码写在注册处。同一个 handler 服务多个动作时（订单的列表/详情/取消共用
//     Orders 一个方法），每个动作的权限码在这里绑定，不靠 handler 内部再判一次。
package routes
