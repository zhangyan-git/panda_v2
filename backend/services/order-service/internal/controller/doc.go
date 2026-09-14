// Package controller 是订单的接入层：解析请求、取身份、把结果写成平台统一的响应体。
//
// 按端拆文件：admin_order.go 是后台端，miniapp_order.go 是小程序端（C 端），
// order_response.go 是两端共用的 model → DTO 映射与错误 → 状态码映射。
//
// 这一层不做业务判断，也有三件不能推给别处的事：
//
//   - **身份来源**。取消人、下单人一律取自令牌，绝不取自请求体。凡是需要「谁在做这件事」
//     的地方，答案只能是 auth.IdentityFromRequest。
//   - **端的闸门**。后台端由装配处（routes.RegisterAdmin）套上「认证 → 平台账号 →
//     实时授权 → 权限码」，小程序端由 requireConsumer 判 realm 与 subject。两端的令牌
//     在 JWT 层是同源的，只有这两道闸门区分得开。
//   - **状态码**。业务层的错误是语义（「这单不在待支付」「设备停用」「问不到设备」），
//     翻成 400/404/409/503 只在这里做一次（writeOrderError），两端共用。
package controller
