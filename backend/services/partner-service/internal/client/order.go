package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	orderv1 "github.com/panda-dev/panda-v2/contracts/proto/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 记一笔设备刷卡订单的三种结局。分开存在，是因为**合作方接下来该做的事不同**：
//
//	ErrDeviceOrderInvalid      改报文再投（字段缺失、饮品编号匹配不到）
//	ErrDeviceOrderNotFound     先把这台机器登记上（设备序列号我们这边不认识）
//	ErrDeviceOrderUnavailable  等一会儿原样重投（故障，不是业务结论）
//
// 前两个是「有结论」，第三个是「没结论」。把没结论的读成「这笔单不合法」是这条路上最容易犯
// 的错：合作方会去改一份本来没错的报文，而真正该做的只是重投——**重投是安全的**，
// third_party_order_no 是订单域的幂等键（见 proto 的说明），同一笔单重投只会拿回同一张。
//
// 三种都不带回订单域的错误串。这一层收下的只有状态码，对外的句子在 controller 那张表里
// （理由见 controller/openapi.go 的文件头：错误串的差别是一次枚举的机会）。
var (
	// ErrDeviceOrderInvalid：订单域不接受这份报文（InvalidArgument）。
	//
	// 「字段缺失」与「drink_code 在这台设备名下匹配不到」在那边是同一个码，这里也合成同一个
	// 结论：对合作方而言都是「这份报文要改」，而区分它们会告诉对方我们的匹配走到哪一步了。
	ErrDeviceOrderInvalid = errors.New("order service rejected the device order")
	// ErrDeviceOrderNotFound：设备序列号查不到（NotFound）。
	//
	// 与上面那条分开，是因为能做的事不同：报文写错了要改报文，而设备号不认识只能先把这台机器
	// 同步进来（它是咖啡机域的 devices.serial_unique，见 proto 的说明）。
	ErrDeviceOrderNotFound = errors.New("device is not registered")
	// ErrDeviceOrderUnavailable：订单域没答上来（连不上、内部错误、超时、应答是空的）。
	ErrDeviceOrderUnavailable = errors.New("order service is unavailable")
)

// 记一笔**取货码**订单（POST /v1/openapi/device/pickup）的几种结局。与上面那三条一样按
// 「合作方接下来该做什么」分，但多出来两条——**这条路会从设备余额里扣钱**，而扣款失败有
// 两种各自明确的结论：
//
//	ErrPickupInvalid         改报文再投（字段缺失、饮品编号匹配不到、这一杯没法定价）
//	ErrPickupDeviceNotFound  先把这台机器登记上（设备序列号我们这边不认识）
//	ErrPickupCodeRejected    换个码（顾客敲错了，或者这台机器没配过码）——重投无用
//	ErrPickupNotEnough       这台机器上余额不够——重投无用，且**不是故障**
//	ErrPickupUnavailable     等一会儿原样重投（故障，不是业务结论）
//
// 后三条必须分开，理由不是好看：前两条要在机器屏幕上变成两句不同的话（「取货码不对」与
// 「余额不足，请充值」），而它们的下一步动作也不同——一个换码、一个充值。把任何一条读成
// ErrPickupUnavailable，合作方就会一直重投一次永远不可能成功的取货。
//
// 五种都不带回订单域或咖啡机域的错误串。对外的句子在 controller 那张表里。
var (
	// ErrPickupInvalid：订单域不接受这份报文（InvalidArgument）。
	//
	// 它也覆盖「这一杯既没有取货码价也没有目录价」：订单域把那条判成了同一个码，而对合作方
	// 而言要做的事确实一样（换一杯，或者来找我们定价）。
	ErrPickupInvalid = errors.New("order service rejected the pickup request")
	// ErrPickupDeviceNotFound：设备序列号查不到（NotFound），只此一种。
	ErrPickupDeviceNotFound = errors.New("device is not registered")
	// ErrPickupCodeRejected：取货码不对，或者这台机器根本没配过码（PermissionDenied）。
	//
	// 咖啡机域那边有意不区分这两种（见它的 deductError），这里也不分：合作方要做的事一样
	// （让顾客重敲一次，或者来找我们把码配上）。
	//
	// 这个串是**内部说法**，与对外的句子不同（那句在 controller 里）：这样一改，泄漏检查
	// 才是真的——两者逐字相同时，「响应体里出现了它」永远为真，那条断言就等于没写。
	ErrPickupCodeRejected = errors.New("the coffee machine rejected the pickup code")
	// ErrPickupNotEnough：这台设备上的咖啡余额不够（FailedPrecondition）。
	//
	// 它是**确定的结论，不是故障**：咖啡机域在同一个事务里判的，拒了就一个字段都没写、
	// 钱一分没动。所以它不能降级成 503——那会让合作方一直重投。
	ErrPickupNotEnough = errors.New("the coffee machine refused the deduction: balance is short")
	// ErrPickupUnavailable：没问到（连不上、内部错误、超时、应答是空的、版本错配）。
	ErrPickupUnavailable = errors.New("pickup service is unavailable")
)

// DeviceOrderInput 是「把这一笔刷卡的既成事实记下来」要交给订单域的全部字段。
//
// # 这里没有 partner_id，也不该有
//
// 调用方的身份**不来自字段**：HTTP 那一侧由 ingress.Guard 从 X-API-Key 对应的那一行解出来
// （密钥在库里，不在报文里），gRPC 这一侧带的是服务令牌。contracts/README.md 写着「没有任何
// 一条 RPC 接受调用方自报的身份字段」，所以订单域看不到这笔设备订单是哪一个合作方带来的
// ——它看得到设备，设备能推到门店，链路到那里为止。
//
// 要把合作方也记进订单里，得先给 proto 加字段或者定一层元数据约定：两件事都不在本轮范围内
// （不能往别人的 proto 里加东西）。这个缺口列在回报里，不在代码里绕过去——例如把 partner_id
// 拼进 remark 是绝对不行的，那是一列给运营看的自由文本，不是身份。
//
// # 字段原样转过去，不做归一
//
// 这一层不 TrimSpace、不换算、不补默认值。归一一个**幂等键**是有代价的：「 A 」与「A」会变成
// 同一张单，而那是「重投」与「两笔不同的订单」的分界。金额同理——它是设备上报的成交价，
// 我们连校验都不做（见下面两条注释）。
type DeviceOrderInput struct {
	// ThirdPartyOrderNo 是对方单号，也是订单域的幂等键。非空，由订单域判。
	ThirdPartyOrderNo string
	// DeviceSerial 是设备序列号（coffee-machine-service 的 devices.serial_unique）。
	//
	// 门店与设备 uuid 都由订单域从这一条推出来：请求里**没有** store_id 这类字段（见 proto），
	// 因为设备是「这台机器属于哪个点位」的唯一来源。收一个调用方填的门店等于让订单挂在设备
	// 并不在的点位上。
	DeviceSerial string
	// DrinkCode 是设备报的饮品编号，**不是我们的 uuid**——机器不可能知道我们的主键。
	//
	// 这个编号怎么变成我们那一行饮品由订单域负责（它转给咖啡机域的 GetDeviceDrink）；
	// 本服务原样转过去，不列菜单、不按编号猜列（理由见 proto 里这个字段的说明：那是饮品库的
	// 布局知识，而且列菜单还会撞上分页——机器上饮品一多就静默匹配不到，那时钱已经收过了）。
	DrinkCode string
	// Amount 是设备上报的成交金额，单位分。为 0 时订单域回退到饮品目录价。
	//
	// **这个值不校验**：钱已经在机器上收过了，我们记的是既成事实。proto 的注释把这条写成了
	// 已知缺口——设备报多少就是多少，跟目录价对不上不会有人发现。
	Amount int64
	// BrewFailed 是设备上报的出饮结果：true=出饮失败。失败的订单照样建（钱收了），
	// 履约状态标成 failed 留人工。
	BrewFailed bool
	// Remark 可空，订单域会拼在「设备刷卡购买」之后。
	Remark string
}

// DeviceOrder 是订单域对一次建单的回答。**只带这三个字段**，不透传 proto（透传会让订单域的
// 字段名变成我们的对外契约，与 MembershipEntitlement 不整个透传是同一条理由，只是这里更短）。
type DeviceOrder struct {
	OrderID string
	OrderNo string
	// Created 为 true 表示这一次真的建了单；false 表示**幂等命中**，返回的是既有那张单。
	//
	// 它不是错误，也不是「什么都没发生」：对方重投是回调类接口的常态，而合作方要能区分
	// 「新建了」与「重投了一次」——这两件事在两边日志里是完全不同的两件事，而如果两种情况
	// 都只回一个空响应，运维就只能看到一串 200。
	Created bool
}

// DeviceOrderCreator 把一笔设备刷卡订单记进订单域（方案 §四）。
//
// 这是本服务**唯一一处会改变别域状态**的调用：其余出向调用（查会员权益、后台的实时鉴权）
// 都只读。所以它带的是服务令牌（platform/auth.WithServiceToken）——这次调用代表平台去记一个
// 事实，不代表合作方，也不代表某个用户（见 DeviceOrderInput 的说明）。
//
// 超时（timeout）由**这一层**加，不靠调用方传进来的 ctx：合作方那头可能永远不超时，而这条
// 链路上每一跳都必须有自己的上限。
type DeviceOrderCreator struct {
	orders  orderv1.OrderServiceClient
	token   string
	timeout time.Duration
}

// NewDeviceOrderCreator 复用调用方那条连接：main 只拨一次，多个调用点共享同一个
// *grpc.ClientConn。
//
// 三个参数都拒绝空值/零值，与这个包里另外两个客户端同一条理由：一个没有令牌的客户端会在
// 运行时被订单域拒掉（Unauthenticated），而那看起来像一次权限事故而不是一次装配错误。
func NewDeviceOrderCreator(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*DeviceOrderCreator, error) {
	if conn == nil {
		return nil, errors.New("order service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("order service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("order service timeout must be positive")
	}
	return &DeviceOrderCreator{
		orders:  orderv1.NewOrderServiceClient(conn),
		token:   token,
		timeout: timeout,
	}, nil
}

// Create 记一笔设备订单。
//
// 服务令牌走 metadata（auth.WithServiceToken），不走请求字段——调用方无法在这个字段上声称
// 自己是别人，这与上面那个「身份不来自字段」是同一条约定的两半。
func (c *DeviceOrderCreator) Create(ctx context.Context, in DeviceOrderInput) (*DeviceOrder, error) {
	if c == nil || c.orders == nil {
		return nil, errors.New("device order creator is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.orders.CreateDeviceOrder(ctx, &orderv1.CreateDeviceOrderRequest{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		DeviceSerial:      in.DeviceSerial,
		DrinkCode:         in.DrinkCode,
		Amount:            in.Amount,
		BrewFailed:        in.BrewFailed,
		Remark:            in.Remark,
	})
	if err != nil {
		return nil, mapDeviceOrderError(err)
	}
	// 一次「建了单却没给单号」的回答是坏的，不是一张没单号的订单：合作方拿不到 orderId /
	// orderNo 就没法把这张单记到自己的账上，而我们也丢掉了唯一一个能查它的线索。当成没问到
	// （重投安全，幂等键在对方单号上），不当成成功。
	if resp == nil || strings.TrimSpace(resp.GetOrderId()) == "" {
		return nil, ErrDeviceOrderUnavailable
	}
	return &DeviceOrder{
		OrderID: resp.GetOrderId(),
		OrderNo: resp.GetOrderNo(),
		Created: resp.GetCreated(),
	}, nil
}

// PickupInput 是「让订单域记一笔取货码订单」要交给它的全部字段。
//
// # 与 DeviceOrderInput 的两处差别，都是有意的
//
//   - **没有金额**：取货码那条路的定价权在订单域（它拿这一杯的取货码价、为 0 回落目录价）。
//     报文里那个金额如果是设备报的，我们收的就不是自己认的价——而这条路上的钱是从**我们
//     自己的设备余额**里扣的。刷卡机那条相反，那边钱在机器上收过了，我们只能记既成事实。
//   - **多一个 PickupPassword**：它是顾客在机器上敲的码，原样转给订单域（比对在持有那一列的
//     咖啡机域里做，见 proto）。本服务不比对、不判空、不 trim——一个字符都不动。
//
// 其余（没有 partner_id、字段原样转、门店从设备推）与 DeviceOrderInput 逐条相同，理由见那边。
type PickupInput struct {
	// ThirdPartyOrderNo 是对方单号，也是这条路唯一的幂等键：它在订单域同时是余额流水上的
	// request_id 与订单上的 third_party_order_no。非空，由订单域判。
	ThirdPartyOrderNo string
	DeviceSerial      string
	DrinkCode         string
	PickupPassword    string
	// Remark 可空，订单域会拼在「取货码购买」之后。
	Remark string
}

// PickupOrder 是订单域对一次取货码建单的回答。字段与 DeviceOrder 一样（三个），但是**另一个
// 类型**：把两个复用成一个会让「取货码这条路上有没有金额」这类问题失去答案。
type PickupOrder struct {
	OrderID string
	OrderNo string
	// Created 为 true 表示这一次真的建了单；false 表示**幂等命中**（那张单早就建好了）。
	//
	// 一次重投要求扣减与建单**同时**幂等：扣减按 request_id 命中，建单按对方单号命中。
	Created bool
}

// CreatePickup 记一笔取货码订单（方案 §四的第二条路）。
//
// 与 Create 一样带服务令牌、由本层加超时：这条调用代表平台去**动一台设备的账**，既不代表
// 合作方也不代表某个用户（机器前面那个人在我们这儿没有账号）。
//
// 它与 Create 共用同一个 DeviceOrderCreator（同一个下游、同一条连接）。分成两个客户端类型
// 的话，装配处要拨两条连接出来，而它们指向的其实是同一个服务。
func (c *DeviceOrderCreator) CreatePickup(ctx context.Context, in PickupInput) (*PickupOrder, error) {
	if c == nil || c.orders == nil {
		return nil, errors.New("device order creator is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.orders.CreatePickupOrder(ctx, &orderv1.CreatePickupOrderRequest{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		DeviceSerial:      in.DeviceSerial,
		DrinkCode:         in.DrinkCode,
		PickupPassword:    in.PickupPassword,
		Remark:            in.Remark,
	})
	if err != nil {
		return nil, mapPickupError(err)
	}
	// 一次「建了单却没给单号」的回答是坏的，理由与 Create 那条相同：合作方拿不到单号就没法
	// 把这一笔记到自己的账上，而我们也丢掉了唯一一个能查它的线索。当成没问到（重投安全）。
	if resp == nil || strings.TrimSpace(resp.GetOrderId()) == "" {
		return nil, ErrPickupUnavailable
	}
	return &PickupOrder{
		OrderID: resp.GetOrderId(),
		OrderNo: resp.GetOrderNo(),
		Created: resp.GetCreated(),
	}, nil
}

// mapPickupError 把 gRPC 状态码翻成上面那五个结论。判据同样是「合作方接下来该做什么」。
//
// 与 mapDeviceOrderError 分开而不是合并：这条路多出来两档（码不对、余额不够），而它们在那条
// 路上根本不存在——合并的结果是一张一半分支永远走不到的 switch。
//
// Unimplemented 与那条一样归在「没问到」：它今天是版本错配（我们比订单域新），让合作方去改
// 一份本来就没错的报文是最没有用的一种回答。
func mapPickupError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return ErrPickupInvalid
	case codes.NotFound:
		return ErrPickupDeviceNotFound
	case codes.PermissionDenied:
		return ErrPickupCodeRejected
	case codes.FailedPrecondition:
		return ErrPickupNotEnough
	default:
		// 含 Unavailable / DeadlineExceeded（我们自己那条线）/ Internal / Unimplemented。
		// 全都归「等一会儿原样重投」：同一笔取货重投不会扣两次（幂等键在对方单号上）。
		return ErrPickupUnavailable
	}
}

// mapDeviceOrderError 把 gRPC 状态码翻成上面那三个结论。判据是「合作方接下来该做什么」。
//
// Unimplemented 归在「没问到」那一档，而不是「你的报文不对」：它今天是版本错配——我们比订单
// 域新，喊了一个对面还没有的 RPC。让合作方去改一份本来就没错的报文，是最没有用的一种回答。
func mapDeviceOrderError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return ErrDeviceOrderInvalid
	case codes.NotFound:
		return ErrDeviceOrderNotFound
	default:
		// 含 Unavailable / DeadlineExceeded（我们自己那条线）/ Internal / Unimplemented。
		// 全都归「等一会儿原样重投」：同一笔单重投不会多出一张，而把一个「没结论」的失败告诉
		// 合作方是「这笔单有问题」，会让他去改一份没错的报文。
		return ErrDeviceOrderUnavailable
	}
}
