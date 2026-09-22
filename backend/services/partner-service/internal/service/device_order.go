package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
)

// 这个文件是开放接口树里**唯一一条写路径**的服务层：把一次设备刷卡回执转成订单域的一次建单
// （线下刷卡机，方案 §四）。
//
// # 它与这个包里另外几条的关系
//
// 别的开放接口只读（查会员权益）。这一条会让订单域**落一行订单**，而且钱已经在机器上收过了
// ——报文进来就是一笔既成事实，没有二次确认的机会。所以这一层的取舍与只读那几条不同：
//
//   - **它不判报文的业务含义**：字段齐不齐、这台设备认不认识、这个饮品编号匹配不匹配，全是
//     订单域的事——那些判断要读设备、读饮品目录，而本服务一个都不复制（见包注释）。在这里
//     抄一份「什么才算一份合法的设备订单」，等于给同一个判断留了两个迟早会不一致的版本。
//   - **它只保证一件事**：这次请求是验过签的（下面那一次 CallerFrom）。
//
// # 幂等由对方单号承担，不由我们
//
// 重投是回调类接口的常态，而幂等键是 third_party_order_no——它在订单域那张表上，不在我们
// 这里。所以这一层既不生成也不改写它，更不自己判重：本服务今天唯一一处「状态」是 Redis 上的
// nonce（见 ingress.nonce），而它挡的是**重放**（同一个请求被原样重发一次），不是**重投**
// （对方自己又发了一次新的、签名合法的请求，比如他没收到上一次的响应）。把两者混起来会让一次
// 正常的重投变成 401，而重投恰恰是这条路上最该被允许的事。

// DeviceOrderRecorder 把设备侧的一笔订单记进订单域——刷卡回执与取货码两条路都在这里。
//
// 接口定义在消费者这一侧（与 EntitlementReader 同一条做法），装配处传真的客户端，测试传桩。
// 返回类型用 client 的那一个而不在本包再造一个：它是这次调用的**形状**，属于适配器。
//
// 两个方法在同一个接口里，是因为它们**去的是同一个下游**（订单域）、走同一条连接、带同一枚
// 服务令牌（见 client.DeviceOrderCreator）。拆成两个依赖会让装配处拨两条指向同一个服务的
// 连接，而它们要解决的问题本来就是一个。
type DeviceOrderRecorder interface {
	Create(ctx context.Context, in client.DeviceOrderInput) (*client.DeviceOrder, error)
	// CreatePickup 记一笔取货码订单：钱从这台设备的咖啡余额里扣（见 pickup.go）。
	CreatePickup(ctx context.Context, in client.PickupInput) (*client.PickupOrder, error)
}

// CreateDeviceOrder 记下一笔设备刷卡订单。
//
// # 本服务不落任何一行
//
// 返回的是订单域的事实，本库里不会因此多出订单、订单行或支付单——订单属于订单域，复制一份
// 到我们这边就是第二份真相（见 Package service 的说明）。
//
// # 下游的错误原样往上走
//
// client 的那三个哨兵在这里**不翻译**：翻成「给合作方看的话」是 HTTP 边界的事，而边界上还
// 要知道这是哪一条接口、要不要进日志（见 controller/openapi.go 的 writeOpenAPIError）。
// 在这里翻一次、在那边再翻一次，就会出现两套句子。
func (s *OpenAPIService) CreateDeviceOrder(ctx context.Context, request dto.DeviceOrderRequest) (dto.DeviceOrderResponse, error) {
	// 身份必须在。理由与 MemberPriceEntitlement 那一处相同，但代价大得多：少了这一句，
	// 一条**没有验过签**的请求（handler 被挂到 Guard 之外、或者装配时忘了包 Guard）会直接
	// 建出一张已支付的订单。那是本服务唯一一种能造成实际损失的错误，所以失败关闭。
	if _, ok := ingress.CallerFrom(ctx); !ok {
		return dto.DeviceOrderResponse{}, ErrCallerMissing
	}
	order, err := s.deviceOrders.Create(ctx, client.DeviceOrderInput{
		// 原样转过去，一个字段都不归一（为什么，见 client.DeviceOrderInput 的说明）。
		ThirdPartyOrderNo: request.ThirdPartyOrderNo,
		DeviceSerial:      request.DeviceSerial,
		DrinkCode:         request.DrinkCode,
		Amount:            request.Amount,
		BrewFailed:        request.BrewFailed,
		Remark:            request.Remark,
	})
	if err != nil {
		return dto.DeviceOrderResponse{}, err
	}
	// created 如实回给对方。这一条不是「顺便带上的字段」：合作方要靠它区分「新建了」与
	// 「重投了一次」，而这两种情况在他那边要做的处理完全不同（前者入账，后者只做核对）。
	return dto.DeviceOrderResponse{
		OrderID: order.OrderID,
		OrderNo: order.OrderNo,
		Created: order.Created,
	}, nil
}
