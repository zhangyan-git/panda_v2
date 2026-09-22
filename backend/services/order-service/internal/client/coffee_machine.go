// Package client 是 order-service 对其他服务的出网调用，全部走 gRPC。
//
// 两种凭据，别混用：
//
//   - 代表基础设施去问事实（本题的 GetDevice、下单时校验设备状态），带服务令牌
//     （platform/auth.WithServiceToken）。它不代表任何用户。
//   - 代表调用者本人去问「他能做什么」（user.go 的 AdminAccessResolver），带他自己的
//     access token（platform/auth.WithAccessToken）。这里绝不能带服务令牌——那等于用
//     基础设施的身份替一个管理员宣称权限。
package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	coffeemachinev1 "github.com/panda-dev/panda-v2/contracts/proto/coffee_machine/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 扣设备余额（取货码那条路）的五种结局。分开存在，是因为**合作方接下来该做的事不同**：
//
//	ErrDeviceBalanceRejected          改报文（咖啡机域不接受这次扣减的参数）
//	ErrDeviceBalanceDeviceMissing     这台机器没登记（重试无用）
//	ErrDeviceBalancePasswordRejected  取货码不对（重试无用，换一个码）
//	ErrDeviceBalanceNotEnough         这台机器上余额不够（重试无用）
//	ErrDeviceBalanceServiceUnavailable 没问到（等一会儿重投）
//
// 前四个是「有结论」，最后一个是「没结论」。把最后一个读成前四个里的任何一个都会让合作方
// 去做一件没用的事（改一份没错的报文、或者相信这台机器没钱了，而其实只是一次抖动）。
//
// **区分「码不对」与「余额不够」不是为了好看**：取货码是顾客在机器前面敲的，这两个结论要
// 在机器屏幕上变成两句不同的话（「码不对」与「余额不足」），而它们的下一步动作也不同。
//
// 五种都不带回咖啡机域的错误串。这一层收下的只有状态码，对外的句子在 partner-service
// 那张表里（理由见那边的 controller/openapi.go：错误串的差别是一次枚举的机会）。
var (
	// ErrDeviceBalanceRejected：咖啡机域不接受这次扣减（InvalidArgument）。
	ErrDeviceBalanceRejected = errors.New("device balance deduction was rejected")
	// ErrDeviceBalanceDeviceMissing：设备在咖啡机域里不在了（NotFound）。
	//
	// 它**只可能是一次竞态**：调用方是拿着刚读回来的设备 uuid 来扣的。真发生了就按「这台
	// 机器没登记」回，让合作方去重新同步设备——重试一次多半也一样。
	ErrDeviceBalanceDeviceMissing = errors.New("device is not registered")
	// ErrDeviceBalancePasswordRejected：取货码不对，或者这台设备根本没配过码（PermissionDenied）。
	//
	// 两种在咖啡机域那边是同一个状态码（那边有意不区分：告诉一个拿着错码的人「这台机器配的
	// 码是空的」是这台设备的内部状态），这里也就不分。
	ErrDeviceBalancePasswordRejected = errors.New("pickup password is not accepted")
	// ErrDeviceBalanceNotEnough：余额不够（FailedPrecondition）。这一单真的做不成，
	// 而且退款那件事不存在——钱根本没出去，咖啡机域一个字段都没写。
	ErrDeviceBalanceNotEnough = errors.New("device balance is not enough")
	// ErrDeviceBalanceServiceUnavailable：没问到咖啡机域（连不上、超时、内部错误）。
	ErrDeviceBalanceServiceUnavailable = errors.New("device balance service is unavailable")
)

// Device 是下单校验用得上的设备事实。
//
// 只带这几个字段，不整个透传 proto 的 Device：「下单时校验设备状态」（方案 5.8）需要的是
// 「这台机器在哪、我们让不让它用、厂商说它通不通」，其余字段（故障描述、上次同步时间）
// 在下单这条路上没有任何判断依据，带进来只会让人以为它们被用上了。
type Device struct {
	ID           string
	StoreID      string
	SerialUnique string
	// active=启用，disabled=停用。这是「我们让不让它用」。
	Status string
	// 厂商上报的在线状态。nil = 从未同步过，与「明确离线」不是一回事。
	VendorOnline *bool
	// 最近一次故障码，空串表示没有故障记录。
	FaultCode string
}

// DeviceReader 读设备与饮品，并且**带着本服务唯一一个对咖啡机域的写口**（DeductBalance，
// 方案 §四的取货码）。
//
// 名字里的 Reader 因此已经名不副实，而这里不改成「Client」是有意的：它指的是**一个下游
// 服务一条连接**这个事实（与 MembershipPlanReader / PaymentCreator 同一条约定），改名要
// 连带动到本文件、payment.go、service 那侧的同名接口与所有引用它的注释，换来的只是名字更
// 准一点；而真正的分界线不在这层皮上——**读与写在这里不共用任何判断**（写只有一条，规则
// 在咖啡机域那边），所以留一个说清楚了的名字比一次半途而废的改名好。
//
// found 为 false 表示咖啡机服务里没有这个 id；err 表示没问到。两者必须分开，理由同
// coffee-machine-service 的 StoreResolver：「不存在」拒单是 404，「问不到」拒单是 503，
// 混成一个错误会让一次下游抖动看起来像用户拿着一个假设备号。
type DeviceReader struct {
	devices coffeemachinev1.CoffeeMachineServiceClient
	token   string
	timeout time.Duration
}

// NewDeviceReader 复用调用方那条连接：main 只拨一次，多个调用点共享同一个 *grpc.ClientConn。
func NewDeviceReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*DeviceReader, error) {
	if conn == nil {
		return nil, errors.New("coffee machine service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("coffee machine service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("coffee machine service timeout must be positive")
	}
	return &DeviceReader{devices: coffeemachinev1.NewCoffeeMachineServiceClient(conn), token: token, timeout: timeout}, nil
}

// Drink 是「某台机器上的一杯饮品」里下单用得上的事实。
//
// 没有 ProductNum / OriginID：机器报的那个编号是哪一列、怎么匹配上的，是饮品库自己的事
// （见 GetDeviceDrink）。本服务拿到的是**已经匹配好的那一杯**，只再要它的身份与目录价。
type Drink struct {
	ID string
	// DeviceID 是这一杯挂在哪台设备上（drinks 一行即「某台设备上的一杯」）。
	//
	// 下单时拿它与请求里的 deviceId 比一次：目录里的一杯属于**某一台机器**，用 A 店的饮品
	// 配 B 店的设备下单，价格与设备对不上而订单看起来是合法的。空串表示这一行还没挂到设备上
	// （库里真有这种历史行），此时不拦——没有可比的设备，不是「挂错了设备」。
	DeviceID string
	// OriginID 是厂商侧饮品 ID，只用于日志与排查——匹配已经由饮品库做完了。
	OriginID string
	// ProductNum 是**机器报的那个编号**，空串表示这一行是后台手工建的、不参与厂商同步。
	//
	// 下单时它落进订单行的 item_code：事后拿机器流水来对账，唯一能对上的就是它（设备单那条
	// 路存的就是报文里那个编号，同一条理由）。它不是主键，也不唯一——同一台设备上两行共用
	// 一个 product_num 是允许的。
	ProductNum string
	Name       string
	// Image 是饮品图，随订单行冻成快照。空串表示没配图，不是「有一张空图」。
	Image string
	// Price 是饮品目录价，单位分。设备报了成交价时它是「标价」，为 0 时才拿来兜底。
	Price int64
	// VipPrice 是这一杯的**会员价**（分），为 0 表示目录没给这杯配会员价——不是「会员价 0 元」。
	//
	// 它与 Price 一起传出来，理由同下面的取货码价：判「这杯有没有会员价、省了多少」要两个
	// 数同时在手，只给一个就得再问一次饮品域。
	VipPrice int64
	// Status 是上下架状态（on_shelf / off_shelf）。
	//
	// 它是**事实，不是判断**：这一杯还卖不卖由调用方按自己那条路决定（见 GetDrink）。
	Status string
	// PickupCodePrice 是这一杯的**取货码价**（分），取货码那条路的第一顺位价，为 0 才回落
	// 到 Price。刷卡机那条路不看它：那边价格是设备报的。
	//
	// 它与 Price 一起传出来，是因为「取货码价没有就回落目录价」这个判断需要两个数同时在手。
	// 只传其中一个，回落就得到调用方再问一次饮品域。
	PickupCodePrice int64
}

// GetBySerial 按机器序列号读一台设备。
//
// 与 Get 是同一件事的两把钥匙：Get 按我们的 uuid 取，这一条按**机器自己认识的那个编号**
// 取。线下刷卡机的回调里只有序列号（那台机器不可能知道我们的主键），所以这条是设备单
// 建单唯一的入口。
//
// 返回语义与 Get 完全一致：found=false 是咖啡机服务里没有这个序列号（NotFound），err 是
// 没问到。两者分开的理由见 DeviceReader 的包注释。
func (r *DeviceReader) GetBySerial(ctx context.Context, serial string) (*Device, bool, error) {
	if r == nil || r.devices == nil {
		return nil, false, errors.New("device reader is not configured")
	}
	// 空序列号在这里就挡掉：咖啡机服务会回 InvalidArgument，而那句话在跨服务的路上会
	// 变成一次 unnecessary 的往返。空串不是「没查到」，是没给。
	if strings.TrimSpace(serial) == "" {
		return nil, false, errors.New("device serial is required")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.GetDeviceBySerial(ctx, &coffeemachinev1.GetDeviceBySerialRequest{SerialUnique: serial})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return deviceFromProto(resp)
}

// GetDeviceDrink 取一台设备上那杯饮品。
//
// 按「设备 uuid + 机器报的编号」查一条，编号落在 product_num 还是 origin_id 由饮品库
// 自己判——那条布局知识不该出那个服务。**不要退化成「列出这台机器上的饮品再自己翻着找」**：
// 列表是分页的，机器上饮品一多就会静默匹配不到，而那时钱已经收过了。
//
// 返回语义与设备那两条一致：found=false 是这台机器上没有这个编号的饮品（NotFound），err 是
// 没问到。两者分开在这里尤其重要——「这杯我们不卖」是对方该换一杯的事实，「没问到」只是
// 一次下游抖动，混成一个会让合作方以为这杯下架了。
func (r *DeviceReader) GetDeviceDrink(ctx context.Context, deviceID, drinkCode string) (*Drink, bool, error) {
	if r == nil || r.devices == nil {
		return nil, false, errors.New("device reader is not configured")
	}
	// 空值在这里就挡掉：饮品格服务会回 InvalidArgument，而那句话在跨服务的路上会变成
	// 一次多余的往返。空串不是「没查到」，是没给。
	if strings.TrimSpace(deviceID) == "" || strings.TrimSpace(drinkCode) == "" {
		return nil, false, errors.New("device id and drink code are required")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.GetDeviceDrink(ctx, &coffeemachinev1.GetDeviceDrinkRequest{
		DeviceId: deviceID, DrinkCode: drinkCode,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if resp == nil || resp.GetId() == "" {
		// 与设备那两条同一条规矩：空应答不是「这杯卖 0 元」，当成问不到。
		return nil, false, errors.New("coffee machine service returned an empty drink")
	}
	return drinkFromProto(resp), true, nil
}

// GetDrink 按**我们的 uuid** 取一杯饮品。它是下单时给饮品行定价的那一条：客户端只给
// drink_id，名称、图片与三级价全从这里现查。
//
// 与 GetDeviceDrink 是两把不同的钥匙，别拿它去替：那一条收的是机器报上来的编号、答案在
// 特定一台设备上；这一条收的是主键。下单的调用方手里只有后者。
//
// 返回语义与设备那几条一致：found=false 是目录里没有这一杯（NotFound），err 是没问到。
// 两者分开在这里格外要紧——「这杯我们不卖」是用户该换一杯的事实，「没问到」是一次下游
// 抖动，混成一个会让一次故障看起来像用户拿着一杯不存在的饮品。
//
// **不判 status**：已下架的饮品照样 found=true 回来，status 在 Drink 上。判它是调用方的事
// （见 service.resolveDrinkLine），因为「能不能卖」在本服务里两条路上答案不同。
func (r *DeviceReader) GetDrink(ctx context.Context, drinkID string) (*Drink, bool, error) {
	if r == nil || r.devices == nil {
		return nil, false, errors.New("device reader is not configured")
	}
	// 空值在这里就挡掉：咖啡机域会回 InvalidArgument，而那句话在跨服务的路上会变成一次
	// 多余的往返。空串不是「没查到」，是没给。
	if strings.TrimSpace(drinkID) == "" {
		return nil, false, errors.New("drink id is required")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.GetDrink(ctx, &coffeemachinev1.DrinkID{Id: drinkID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if resp == nil || resp.GetId() == "" {
		// 与设备那两条同一条规矩：空应答不是「这杯卖 0 元」，当成问不到。
		return nil, false, errors.New("coffee machine service returned an empty drink")
	}
	return drinkFromProto(resp), true, nil
}

// drinkFromProto 把 proto 的 Drink 收成本服务用得上那几个字段。
//
// 两条读路径（GetDeviceDrink / GetDrink）共用它，是为了**不让同一个结构体在两处装出两个
// 样子**：设备单那条路今天只用得上 Price 与 PickupCodePrice，但它在同一个 Drink 上——
// 少填几个字段的代价是下一个人读到一个「有时有图有时没图」的结构体，然后按「反正用不上」
// 处理掉一次真实的差异。
func drinkFromProto(resp *coffeemachinev1.Drink) *Drink {
	return &Drink{
		ID:              resp.GetId(),
		DeviceID:        resp.GetDeviceId(),
		OriginID:        resp.GetOriginId(),
		ProductNum:      resp.GetProductNum(),
		Name:            resp.GetProductName(),
		Image:           resp.GetProductImg(),
		Price:           resp.GetPrice(),
		VipPrice:        resp.GetVipPrice(),
		PickupCodePrice: resp.GetPickupCodePrice(),
		Status:          resp.GetStatus(),
	}
}

// Get 读一台设备。
func (r *DeviceReader) Get(ctx context.Context, deviceID string) (*Device, bool, error) {
	if r == nil || r.devices == nil {
		return nil, false, errors.New("device reader is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.GetDevice(ctx, &coffeemachinev1.DeviceID{Id: deviceID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return deviceFromProto(resp)
}

// DeductBalanceInput 是一次扣设备余额要交给咖啡机域的全部字段。
//
// 它没有金额的正负号约定：Amount 是**正数**（扣多少），符号由扣减那一侧补上（流水记负数）。
// 在这里取反一次、那边再取反一次，最后就会变成加钱。
type DeductBalanceInput struct {
	// DeviceID 是设备 uuid（不是我，也不是序列号）：钱挂在这一行上。
	DeviceID string
	// Amount 是扣减金额，正数，单位分。
	Amount int64
	// RequestID 是幂等键。取货码那条路上它**就是对方单号**——一个键两个身份，见
	// service.CreatePickupOrder 的说明。空值在这里就挡下：空串会绕过咖啡机域那条
	// request_id 非空的唯一索引，于是重投会扣两次。
	RequestID string
	// PickupPassword 是顾客敲的取货码，原样转下去（本层不 trim、不判空，理由见 proto）。
	PickupPassword string
	// Remark 进余额流水的备注，给人看的。
	Remark string
}

// DeductBalanceResult 是一次扣减的回答。
type DeductBalanceResult struct {
	// BalanceAfter 是扣完之后的余额（分）；Applied=false 时是当初那一次记下的余额。
	BalanceAfter int64
	// Applied 为 false 表示这个 RequestID 已经扣过了，本次一个字段都没写。
	//
	// **它不是失败**：调用方要当成成功继续往下走（把订单建出来）。把它翻成错误的话，
	// 一次「钱已经扣了、单还没建」的重投会让这一单永远建不出来。
	Applied bool
	// Amount 是这一次**实际扣掉**的金额（正数，分）；Applied=false 时是当初那一次扣的。
	//
	// 它是这条路上唯一可信的金额：身份重投时它取自流水，可以与本服务自己算出来的价不等
	// （两次之间这一杯被改过价）。调用方要用它记账——订单上的金额与流水上的对不上，
	// 那笔差额没有任何一处能解释。
	//
	// **0 表示对面没给这一格**（版本错配：咖啡机域还是加这格之前的版本）。调用方按 0
	// 处理成「拿不到当初的金额」，退回到自己算的价，而不是当成一次 0 元扣款。
	Amount int64
}

// DeductBalance 从一台设备的咖啡余额里扣一笔（取货码那条路，方案 §四）。
//
// # 它是本服务唯一的写调用
//
// 其余出向调用（读设备、读饮品、读套餐、发起支付、取微信身份）里，只有发起支付与本方法会
// 改变别域状态。它带服务令牌，不代表任何用户——机器前面那个人在我们这儿没有账号。
//
// # 顺序与失败的含义
//
// 调用方必须**先扣钱、后建单**（见 service.CreatePickupOrder）：反过来的话中间断了就是
// 「建了单没扣钱」，白送一杯。
//
// # 应答为空是坏的
//
// 与读那几条不同，这里没有「找不到」的中间态：一次没有错的应答必须带着 applied 这个结论。
// 但 proto3 的 bool 没有 presence，读不出「没给」，所以这里只挡空应答结构本身——真正要防的
// 「扣没扣」由调用方按 applied 继续判断，而重投是安全的（幂等键在对方单号上）。
func (r *DeviceReader) DeductBalance(ctx context.Context, in DeductBalanceInput) (*DeductBalanceResult, error) {
	if r == nil || r.devices == nil {
		return nil, errors.New("device reader is not configured")
	}
	// 空值在这里挡掉：咖啡机域会回 InvalidArgument，而那句话在跨服务的路上会变成一次多余的
	// 往返。空串不是「没查到」，是没给。**验证码不在这里判**：它是不是空的只有持有那一列的
	// 服务说了算（见 DeductBalanceInput）。
	if strings.TrimSpace(in.DeviceID) == "" {
		return nil, errors.New("device id is required")
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return nil, errors.New("request id is required")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.DeductDeviceBalance(ctx, &coffeemachinev1.DeductDeviceBalanceRequest{
		DeviceId:       in.DeviceID,
		Amount:         in.Amount,
		RequestId:      in.RequestID,
		PickupPassword: in.PickupPassword,
		Remark:         in.Remark,
	})
	if err != nil {
		return nil, mapDeductError(err)
	}
	if resp == nil {
		// 没有应答结构就是没问到，不是「扣了 0 元」——当成失败让调用方重投（幂等安全）。
		return nil, ErrDeviceBalanceServiceUnavailable
	}
	return &DeductBalanceResult{
		BalanceAfter: resp.GetBalanceAfter(),
		Applied:      resp.GetApplied(),
		Amount:       resp.GetAmount(),
	}, nil
}

// mapDeductError 把咖啡机域的状态码翻成上面那五个结论。
//
// Unimplemented 与读那几条一样归在「没问到」：它今天是版本错配——我们比咖啡机域新，喊了
// 一个对面还没有的 RPC。让它看起来像「这次扣减的报文不对」会把一次部署问题变成一句误导
// 合作方的话。
func mapDeductError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return ErrDeviceBalanceRejected
	case codes.NotFound:
		return ErrDeviceBalanceDeviceMissing
	case codes.PermissionDenied:
		return ErrDeviceBalancePasswordRejected
	case codes.FailedPrecondition:
		return ErrDeviceBalanceNotEnough
	default:
		// 含 Unavailable / DeadlineExceeded（我们自己那条线）/ Internal / Unimplemented。
		// 全都归「等一会儿重投」：同一笔扣减重投不会扣两次（幂等键在对方单号上）。
		return ErrDeviceBalanceServiceUnavailable
	}
}

// deviceFromProto 把 proto 的 Device 读成下单用得上的事实。
//
// Get 与 GetBySerial 共用它：两把钥匙问的是同一件事，读出来的事实必须逐字段一致——各写
// 一份的后果是「按 uuid 查得到状态、按序列号查出来永远是在线」，而两条路的下游都是下单。
func deviceFromProto(resp *coffeemachinev1.Device) (*Device, bool, error) {
	if resp == nil || resp.GetId() == "" {
		// 应答里没有 id 又不算 NotFound：这不可能是一次正常回答，当成问不到，
		// 别让一个空应答被读成「设备存在且状态为空」。
		return nil, false, errors.New("coffee machine service returned an empty device")
	}
	device := &Device{
		ID:           resp.GetId(),
		StoreID:      resp.GetStoreId(),
		SerialUnique: resp.GetSerialUnique(),
		Status:       resp.GetStatus(),
		FaultCode:    resp.GetLastFaultCode(),
	}
	// 用 proto 的 presence 而不是零值：optional bool 的「没给」和「给了 false」是两件事，
	// 读成 bool 就把「从未同步过」压成了「离线」，下单校验会拿一个我们其实不知道的事实
	// 去拒单。
	if resp.VendorOnline != nil {
		online := resp.GetVendorOnline()
		device.VendorOnline = &online
	}
	return device, true, nil
}
