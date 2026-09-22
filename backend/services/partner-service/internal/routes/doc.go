// Package routes 把 controller 挂到 HTTP 路由上。
//
// 这个包里有**两条注册入口，约定相反**（admin.go 与 openapi.go）：
//
//   - RegisterAdmin —— 后台治理树（/v1/admin/*）。每条路由都挂认证与权限码，传 nil 校验器
//     时全 401（失败关闭，见 admin.go 的 protect）。
//   - RegisterOpenAPI —— 开放接口树（/v1/openapi/*）。**故意不挂我们的认证**：合作方没有
//     我们的令牌，它带的是自己算的签名（X-API-Key + X-Signature）。整棵树被
//     ingress.Guard 包着，验签 → 时间窗 → nonce → 启停 → 过期 → 白名单 → 限流七道都在
//     那一段里，之后 handler 才拿得到合作方身份。
//
// 两者**不合并成一个 Register**：合并之后 admin 那条「校验器为 nil 就全 401」的保证会被
// 开放接口树的存在稀释，而后者恰恰需要「校验器为 nil 也能正常工作」——那正是它的常态。
// 两个函数、两次调用，各自把自己那条规矩写死在自己头上。这与 payment-service 的
// 回调树 / 后台树是同一条理由（见那边的 routes/doc.go）。
//
// 其余约定与别的服务一致：路径注册顺序从长到短（底层是 gorilla/mux，按注册顺序取第一个
// 匹配），装配处注入依赖，本包不自己 new 任何东西。
package routes
