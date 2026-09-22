// Package controller 是支付服务的接入层：解析请求、把结果写成响应。
//
// # 这里有两棵树，约定恰好相反
//
//   - **渠道树**（渠道打进来的两条路：/v1/payments/callback/{channelCode} 见 callback.go，
//     /v1/payments/return/{channelCode} 见 return.go）。
//   - **后台树**（/v1/admin/payments 与 /v1/admin/payments/{paymentNo}，见
//     admin_payment.go），只读、挂身份与权限码。早先这条前缀下还有两个可写的配置树
//     （payment-methods / payment-channels），随渠道表一起删了。
//
// 两棵树在同一个包里，但它们对「身份」「响应信封」「能不能改状态」这三个问题的答案**正好
// 相反**，所以下面这份清单是按树分开写的，不是按包写的：
//
// 渠道树——
//
//   - **没有身份**。请求来自渠道（或由渠道送回来的浏览器），不来自用户，所以没有
//     auth.Middleware、没有权限码、也没有「身份只从令牌取」那条规矩。它的替代品是验签：
//     `service.HandleNotification` 与 `service.HandleReturn` 都在碰报文里任何一个字段之前
//     先验签，而路由是**故意不挂认证**的——挂了也没用，渠道不会带我们的令牌，只会带它自己
//     的签名。
//   - **不套 api.Success/api.Error**。那套信封是我们自己的客户端协议，渠道不认。回调的应答
//     要按渠道约定的形状写（`provider.Ack`，见 writeCallbackAck），回跳的应答是给浏览器看
//     的（见 writeReturnPage）。两条都不是我们的信封。
//   - **改状态的两条规矩相反**。回调改支付状态，而且所有改动都在 service 的事务里；这一层
//     只负责把原始报文原样递给它、把它的结论翻成应答。**回跳一行都不写**
//     （见 service.HandleReturn），这不是这一层的自觉，是那条路上根本没有写方法可调。
//
// 后台树——
//
//   - **有身份**。每条路由都过 auth.Middleware → adminOnly → authz.Middleware →
//     RequirePermission(payment:read)，由 routes.RegisterAdmin 在建表时逐条套上（见
//     routes/admin.go）。漏装配的表现是**全 401**，不是全放行。
//   - **套 api.Success/api.Error**。admin-web 的响应拦截器解的就是这套信封。这一层的
//     错误 → 状态码映射集中在 admin_response.go 的 writeAdminPaymentError。
//   - **同样不改状态，而且是更强的那种「不改」**：这条树上一条写路由都没有。支付单是钱的
//     既成事实，开写入口要先有退款与对账的语义，而它们本轮没做。
//
// # 这一层在小程序侧仍然是空的
//
// 小程序没有 payment 的 HTTP 面：发起支付是 order-service 编排的（它走 gRPC 调本服务，
// 见 internal/rpc），客户端拿到的支付参数是那次调用的返回值。商户侧也没有——本切片只做
// admin-web。退款那两个前缀留给后面的切片。
//
// 两棵树的错误 → 状态码映射各自集中在一处（payment_response.go 与 admin_response.go），
// 理由与订单服务一样：同一个错误在不同入口必须是同一个码，否则前端得按接口记两套规矩。
package controller
