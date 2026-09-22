package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
)

// 这个文件是开放接口树上的第二条写路径：把一次取货码回执转成订单域的一次建单——而这一次
// **会从设备余额里扣钱**（方案 §四）。
//
// # 它与 device_order.go 那一条的关系
//
// 两条路的形状完全一样（验过签的报文进来就是一笔既成事实、本服务不判业务含义、幂等键在订单
// 域），差别只有一处，而那一处值得单独一个文件：刷卡回执的钱已经在机器上收过了，我们只是
// 记账；取货码这条路的钱是**我们从自己账上扣的**，所以它多了一个会失败、且失败原因要对顾客
// 讲清楚的动作。那几条结论的对外说法在 controller 那张表里，这里只是原样把它往上放。

// CreatePickupOrder 记下一笔取货码订单（顾客在机器上敲码、钱从这台设备的咖啡余额里扣）。
//
// # 本服务不落任何一行，也不做任何判断
//
// 与 CreateDeviceOrder 逐条相同：返回的是订单域的事实，本库里不会多出订单、订单行或余额
// 变动——那些都属于订单域与咖啡机域。字段齐不齐、这台设备认不认识、码对不对、钱够不够，
// 全是那两个域的事（本服务一个判断都不复制：抄一份过来就有两个迟早不一致的版本）。
//
// # 身份必须在
//
// 理由与 CreateDeviceOrder 那一条相同，而**代价更大**：少了这一句，一条没有验过签的请求会
// 直接扣掉一台设备的钱并建出一张已支付的订单——那是本服务唯一一种能造成实际损失的错误。
// 所以失败关闭。
func (s *OpenAPIService) CreatePickupOrder(ctx context.Context, request dto.PickupRequest) (dto.PickupResponse, error) {
	if _, ok := ingress.CallerFrom(ctx); !ok {
		return dto.PickupResponse{}, ErrCallerMissing
	}
	order, err := s.deviceOrders.CreatePickup(ctx, client.PickupInput{
		// 原样转过去，一个字段都不归一。取货码尤其如此：它是顾客敲进去的一串字符，比对在
		// 持有那一列的服务里做（见 client.PickupInput）。
		ThirdPartyOrderNo: request.ThirdPartyOrderNo,
		DeviceSerial:      request.DeviceSerial,
		DrinkCode:         request.DrinkCode,
		PickupPassword:    request.PickupPassword,
		Remark:            request.Remark,
	})
	if err != nil {
		// 五种结论原样上抛，这里一条都不翻译（理由见 CreateDeviceOrder）：翻成「给合作方看
		// 的话」是 HTTP 边界的事，而边界上还要知道这是哪一条接口、要不要进日志。
		return dto.PickupResponse{}, err
	}
	return dto.PickupResponse{
		OrderID: order.OrderID,
		OrderNo: order.OrderNo,
		// created 如实回给对方：他靠它区分「这一杯刚扣了钱」与「上一次已经扣过了」——
		// 这两件事在机器维护记录里完全不同。
		Created: order.Created,
	}, nil
}
