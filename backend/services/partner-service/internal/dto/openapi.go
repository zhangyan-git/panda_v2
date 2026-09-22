package dto

// 这个文件是**开放接口**的对外形状（合作方看到的那几个字段）。它与 partner.go 分开，因为
// 两者的读者和冻结程度不一样：
//
//   - partner.go 的形状是给 admin-web 看的，跟着后台页面一起改，改坏了影响我们自己；
//   - 这里的形状是**对外契约**，写在对接文档里，合作方已经按它写了代码。改一个字段名等于
//     通知所有对接方改代码，所以它单独一个文件、单独一段说明——改动前要先想一遍对面。
//
// 与后台那些 dto 一样用 camelCase（对接文档里也是这么写的），但**没有**后台那种内嵌
// PageItem 之类的包裹：开放接口的列表形态还没出现（今天只有两条：一条查询、一条设备订单
// 回执，两条都是单对象），等有了再定。

// EntitlementResponse 是查会员权益那一条的 data。
//
// 字段与 client.MembershipEntitlement 一一对应，多两个身份字段：
//
//	userId    原样回显请求里那个 ID（归一成 uuid 之后的形式）。合作方一次批量查询里对不上
//	          是哪一个用户时，靠它对齐——响应里只有一组布尔值时，那是最容易出的错。
//	partnerId 发起这次查询的合作方。**它是回显，不是下游的事实**：会员服务看不到合作方身份
//	          （见 client.MembershipEntitlementReader 的说明），所以这个值由我们填。它的用途
//	          是让合作方在排查「这次查询是不是我发的」时能对上自己的调用日志——两边对同一个
//	          requestId 记的是同一件事。
type EntitlementResponse struct {
	UserID    string `json:"userId"`
	PartnerID string `json:"partnerId"`
	// Active 此刻是不是有效会员（冻结中的会员是 false）。
	Active bool `json:"active"`
	// GrantsMemberPrice 此刻能不能**直接**按会员价算（只有年度会员 auto 为真）。
	GrantsMemberPrice bool `json:"grantsMemberPrice"`
	// MemberPriceMode 是 auto（本人自动享）或 coupon（靠会员价体验券）；不是会员时空串。
	MemberPriceMode string `json:"memberPriceMode"`
	// PlanCode / PlanName 是成交快照上的套餐（后台改过套餐名不影响这里）。
	PlanCode string `json:"planCode"`
	PlanName string `json:"planName"`
	// ExpireAtUnix 是到期时间（Unix 秒）；不是会员时 0。
	ExpireAtUnix int64 `json:"expireAtUnix"`
}

// DeviceOrderRequest 是设备刷卡回执（POST /v1/openapi/device/sync-order）的请求体。
//
// # 六个字段，一个不多，与 order/v1 的 CreateDeviceOrderRequest 一一对应
//
// proto 那一份是权威（contracts/proto/order/v1/order.proto 里每个字段都带理由），这里是它在
// HTTP 边界上的形状。解码用 DisallowUnknownFields（见 controller 的 decodeOpenAPIBody），
// 所以多出来的键是 400，不是被静默忽略。
//
// # 这里没有 partnerId，也不该有
//
// 合作方身份来自**凭据**，不来自字段：谁发的由 X-API-Key 对应的那一行决定（见
// ingress.Caller），报文里就算带了 partnerId 也不算数。让这个字段存在才是真正危险的——
// 它会与验签出来的身份不一致，而「以谁的身份」与「报文自称是谁」不一致时，写下来的每一行
// 数据都要重新解释一遍。contracts/README.md 写着内部 RPC 不接受调用方自报的身份字段，
// 这条从 HTTP 边界就开始成立：报文里根本没有这个位置（带了的话，它是个未知字段，400）。
//
// # 字段名是对外契约
//
// camelCase，与这个包里另外那一份一致。改一个名字等于通知所有对接方改代码。
type DeviceOrderRequest struct {
	// ThirdPartyOrderNo 是对方单号，幂等键，非空。
	ThirdPartyOrderNo string `json:"thirdPartyOrderNo"`
	// DeviceSerial 是设备序列号，非空。门店与设备 uuid 由订单域从它推出来。
	DeviceSerial string `json:"deviceSerial"`
	// DrinkCode 是设备报的饮品编号，**不是我们的 uuid**（机器不可能知道我们的主键）。
	//
	// 它怎么变成我们那一行饮品是订单域的事（它转给咖啡机域的 GetDeviceDrink(device_id,
	// drink_code)）——那个编号落在 drinks 的哪一列取决于这家厂商当初的同步来源，是饮品库的
	// 布局知识。本服务不列菜单、不按编号猜列，原样转过去（见 proto 里这个字段的说明）。
	DrinkCode string `json:"drinkCode"`
	// Amount 是设备上报的成交金额，单位**分**；为 0 时订单域回退到饮品目录价。
	Amount int64 `json:"amount"`
	// BrewFailed 是出饮结果：true=出饮失败。失败的单照样建（钱收了），履约标成 failed 留人工。
	BrewFailed bool `json:"brewFailed"`
	// Remark 可空，订单域会拼在「设备刷卡购买」之后。
	Remark string `json:"remark"`
}

// DeviceOrderResponse 是那一条的 data。
//
// 三个字段，都是订单域给回来的事实的投影（见 client.DeviceOrder），**没有本服务加工过的
// 任何东西**：本服务不建单、不改单，也不给这笔订单补一个自己的编号。
//
//	orderId / orderNo  合作方拿它把这张单记到自己的账上（两个都给：单号是他对账时用的，
//	                   uuid 是我们这边的排查入口）。
//	created            true=这一次真的建了单；false=**幂等命中**，返回的是既有那张单。
//	                   它不是错误（HTTP 仍是 200）：对方重投是这类接口的常态，而他需要能区分
//	                   「新建了」与「重投了一次」——这两件事在运维日志里完全不同。
type DeviceOrderResponse struct {
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	Created bool   `json:"created"`
}

// PickupRequest 是取货码回执（POST /v1/openapi/device/pickup）的请求体。
//
// # 五个字段，与 order/v1 的 CreatePickupOrderRequest 一一对应
//
// proto 那一份是权威。这里同样是 camelCase、同样用 DisallowUnknownFields 解码（多一个键是
// 400，不是被静默忽略），同样**没有 partnerId**：合作方身份来自凭据（X-API-Key 对应的那一行），
// 报文里就算带了也不算数（理由见 DeviceOrderRequest 的说明）。
//
// # 与 DeviceOrderRequest 相比少一个、多一个
//
//   - **没有 amount**：取货码那条路由订单域定价（这一杯的取货码价，为 0 回落目录价）。刷卡机
//     那条收设备报的价，是因为钱在机器上收过了；这条路的钱是从**我们自己的设备余额**里扣的，
//     收对方报的价等于让它决定我们扣多少。
//   - **多一个 pickupPassword**：顾客在机器上敲的那个码，原样转下去。本服务不比、不判空、
//     不 trim——比对在持有那一列的咖啡机域里做（见 proto）。
type PickupRequest struct {
	// ThirdPartyOrderNo 是对方单号，**这条路唯一的幂等键**，非空。
	//
	// 它在订单域同时是余额流水上的 request_id 与订单上的 third_party_order_no：一次重投要
	// 这两件事同时不生效，靠的就是它们是同一个值。所以**每次取货都必须是一个新的、唯一的值**——
	// 两台机器上先后发生的两次取货共用一个单号的话，第二次会被当成重投，那一杯就白出了。
	ThirdPartyOrderNo string `json:"thirdPartyOrderNo"`
	// DeviceSerial 是设备序列号，非空。钱扣的是这台设备，门店也由它推出来。
	DeviceSerial string `json:"deviceSerial"`
	// DrinkCode 是设备报的饮品编号，**不是我们的 uuid**。
	DrinkCode string `json:"drinkCode"`
	// PickupPassword 是顾客敲的取货码，非空由咖啡机域判（空码与错码在那边是同一个结论）。
	PickupPassword string `json:"pickupPassword"`
	// Remark 可空，订单域会拼在「取货码购买」之后。
	Remark string `json:"remark"`
}

// PickupResponse 是那一条的 data。
//
// 三个字段，与 DeviceOrderResponse 逐字相同——**不是同一个类型**是有意的：两条路径的请求形状
// 不同（一条有金额、一条有取货码），把响应并成一个类型会让人以为请求也能并。
//
//	orderId / orderNo  合作方拿它把这一笔记到自己的账上。
//	created            true=这一次真的建了单并扣了钱；false=幂等命中，扣减与建单都没有再发生。
type PickupResponse struct {
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	Created bool   `json:"created"`
}
