// Package routes 把 controller 挂到 HTTP 路由上。
//
// 这个包里有**两条注册入口，约定相反**（payment.go 与 admin.go）：
//
//   - RegisterPayment —— 渠道打进来的路（支付结果通知 POST /v1/payments/callback/…、
//     协议变更通知 POST /v1/payments/agreement-notify/…、结果页回跳 GET /v1/payments/return/…），
//     **故意都不挂认证**。渠道没有我们的令牌，它带的是自己的签名，所以「认证」在这条路上的
//     等价物是验签，而验签发生在 service 里、发生在碰报文任何一个字段之前。挂 auth.Middleware
//     只会让每一次回调都 401，而且是**安静地**失败：渠道那边看到的是「对面没收下」，我们这边
//     看到的是一张永远停在 pending 的支付单（签约那条则是永远停在待签约的协议）。
//
//   - RegisterAdmin —— 后台只读树，**每条路由都挂认证与权限码**，且传 nil 校验器时全 401
//     （失败关闭，见 admin.go 的 protect）。
//
// 两者**不合并成一个 Register**：合并之后 admin 那条「校验器为 nil 就全 401」的保证会被
// 回调树的存在稀释，而回调树恰恰需要「校验器为 nil 也能正常工作」——那正是它今天的状态。
// 两个函数、两次调用，各自把自己那条规矩写死在自己头上。
//
// 其余约定与别的服务一致：路径注册顺序从长到短（底层是 gorilla/mux，按注册顺序取第一个
// 匹配），装配处注入依赖，本包不自己 new 任何东西。
package routes
