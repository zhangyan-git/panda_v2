import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 商户端订单接口。字段名与 order-service dto（internal/dto/order.go、after_sale.go）的
 * json tag 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处，否则 TypeScript 不
 * 报错，只是整列空白。
 *
 * 金额一律是「分」的整数（int64 到了 JS 里就是 number）。这一层不换算，页面按元展示。
 *
 * 这里没有「按门店筛」的入参：能看见哪些单由服务端按令牌实时解析出来的数据范围决定。
 * 售后**不在商户端范围内**——它不是独立入口，只作为订单详情的一部分随订单一起受范围约束。
 */

// ——— 枚举 ———
// 取值来自 order-service 的 model 常量（各表的 CHECK 约束），不是接口给的——接口回的
// 就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而这里没登记，界面上
// 就退回显示原始码。文案表在 services/orderLabels.ts。

export type OrderStatus =
  | 'pending_payment'
  | 'paid'
  | 'completed'
  | 'cancelled'
  | 'expired'
  | 'refunding'
  | 'refunded';

export type FulfillmentStatus =
  | 'none'
  | 'pending'
  | 'making'
  | 'ready'
  | 'completed'
  | 'failed'
  | 'cancelled';

export type OrderSource = 'miniapp' | 'screen_qr';
export type OrderLineType = 'drink' | 'addon' | 'membership';
export type PaymentLineStatus = 'reserved' | 'succeeded' | 'failed' | 'released' | 'reversed';

export type ActorType = 'user' | 'merchant' | 'admin' | 'system';
export type AggregateType = 'order' | 'order_line' | 'after_sale' | 'payment_line';

export type AfterSaleStatus =
  | 'pending'
  | 'approved'
  | 'rejected'
  | 'refunding'
  | 'refunded'
  | 'failed'
  | 'cancelled';

export type AfterSaleScope = 'all' | 'drink' | 'addon' | 'membership';

// ——— 订单 ———

/** 列表里的一行。不带行明细与出资分摊——那是详情的事。 */
export type OrderSummary = {
  id: string;
  orderNo: string;
  userId: string;
  source: OrderSource;
  status: OrderStatus;
  fulfillmentStatus: FulfillmentStatus;
  storeId: string | null;
  storeName: string;
  deviceId: string | null;
  deviceNo: string;
  originalAmount: number;
  discountAmount: number;
  payableAmount: number;
  paidAmount: number;
  refundedAmount: number;
  /**
   * 支付方式的 code（payment-service 目录里的，如 ums_h5_alipay / coffee_bean）。
   *
   * 类型是 string 而不是联合类型：这张值域表由支付服务持有、会随后端加支付方式而长，把六条
   * code 抄成联合类型只会让「后端的第七种」在编译期变成一个假错误。设备单还会在这里写
   * card_pay / pickup_code（订单侧自造的标签，没有支付单）。
   *
   * 界面上按已知的那几条翻成中文，认不出来的原样回显（见 orderLabels.paymentMethodLabel）。
   */
  paymentMethod: string;
  paymentNo: string;
  paidAt: string | null;
  finishedAt: string | null;
  cancelledAt: string | null;
  cancellationReason: string;
  expiresAt: string | null;
  remark: string;
  /** 只读派生：这一单有没有饮品行 / 加购行 / 会员行，列表「构成」列用。 */
  hasDrinkLine: boolean;
  hasAddonLine: boolean;
  hasMembershipLine: boolean;
  /** 下单时承诺赠送的福卡张数。 >0 时审核退款要有人确认没抽过奖（商户端只读，只展示）。 */
  fortuneCardsExpected: number;
  /**
   * 取杯号：这一单饮品行那个短号。没付成功或没有饮品行时为 null。
   *
   * 用户侧叫取杯号，取杯口屏幕上叫取杯码，是**同一个值**，不是凭据（见 migrations/order）。
   */
  pickupCode: string | null;
  createdAt: string;
  updatedAt: string;
};

/** 订单行。 */
export type OrderLine = {
  id: string;
  lineNo: number;
  lineType: OrderLineType;
  itemId: string | null;
  itemCode: string;
  itemName: string;
  itemImage: string;
  quantity: number;
  originalUnitPrice: number;
  unitPrice: number;
  priceDiscountAmount: number;
  discountAmount: number;
  payableAmount: number;
  couponId: string | null;
  couponDiscountAmount: number;
  specs: unknown;
  selectionSnapshot: unknown;
  campaignId: string | null;
  campaignSnapshot: unknown;
  membershipPlanSnapshot: unknown;
  deviceId: string | null;
  deviceOrderNo: string;
  fulfillmentTaskNo: string;
  /** 取杯号，只有饮品行有，支付成功时生成。 */
  pickupCode: string | null;
  remark: string;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一次出资分摊。
 *
 * 没有渠道流水号：`provider_transaction_id` 后端有意不映射（对账凭据，要看去 payment-service
 * 查），所以这里既没有这个字段，页面也不该去找。
 */
export type OrderPaymentLine = {
  id: string;
  lineNo: number;
  /** 支付方式的 code，与 orders.payment_method 同一个值（词表已退场，见 migrations/order）。 */
  lineType: string;
  amount: number;
  status: PaymentLineStatus;
  paymentNo: string;
  failureCode: string;
  accountEntryId: string | null;
  succeededAt: string | null;
  reversedAt: string | null;
  createdAt: string;
};

/** 一条状态流水。前两个字段一起回答「谁的状态从什么变成了什么」。 */
export type OrderTransition = {
  id: string;
  aggregateType: AggregateType;
  aggregateId: string;
  fromStatus: string;
  toStatus: string;
  reason: string;
  actorType: ActorType;
  actorId: string | null;
  createdAt: string;
};

/** 按行退时被退的那一行（整单退为 null）。 */
export type AfterSaleLineRef = {
  id: string;
  lineNo: number;
  lineType: OrderLineType;
  itemName: string;
  quantity: number;
  payableAmount: number;
};

/**
 * 售后单。后三个字段是只读派生（那三个数不在售后单上，是从订单和订单行上取的），
 * 审核福卡规则要看的就是它们。
 */
export type AfterSale = {
  id: string;
  afterSaleNo: string;
  orderId: string;
  orderNo: string;
  userId: string;
  /** 目前只有 refund。 */
  type: string;
  scope: AfterSaleScope;
  orderLineId: string | null;
  status: AfterSaleStatus;
  reason: string;
  /** 用户上传的凭证图片 URL 列表，可能是 null。 */
  images: string[] | null;
  refundAmount: number;
  refundNo: string;
  failureCode: string;
  reviewedBy: string | null;
  reviewedAt: string | null;
  reviewRemark: string;
  refundedAt: string | null;
  createdAt: string;
  updatedAt: string;
  fortuneCardsExpected: number;
  fortuneCardSnapshot: unknown;
  orderLine: AfterSaleLineRef | null;
};

/** 订单详情 = 列表那一行 + 明细 + 出资 + 流水 + 这一单的售后记录。 */
export type OrderDetail = OrderSummary & {
  sceneToken: string;
  membershipId: string | null;
  membershipSnapshot: unknown;
  fortuneCardSnapshot: unknown;
  lines: OrderLine[];
  paymentLines: OrderPaymentLine[];
  transitions: OrderTransition[];
  afterSales: AfterSale[];
};

// ——— 接口 ———

/**
 * 订单筛选。只认 status / orderNo / source / createdFrom / createdTo——后端
 * MerchantOrderQuery 里**没有 keyword**，orderNo 是精确匹配（`=`），所以调用前要 trim，
 * 也不要指望它做模糊。
 */
export type MerchantOrderQuery = PageQuery & {
  orderNo?: string;
  status?: OrderStatus;
  source?: OrderSource;
  /** 创建时间的闭开区间 [createdFrom, createdTo)，必须是带时区的 RFC3339（用 toRFC3339 转）。 */
  createdFrom?: string;
  createdTo?: string;
};

export async function listOrders(params?: MerchantOrderQuery) {
  return request<PageResult<OrderSummary>>('/api/v1/merchant/orders', { params });
}

/** 订单详情。范围外与不存在都回 404，服务端不区分这两件事。 */
export async function getOrder(id: string) {
  return request<OrderDetail>(`/api/v1/merchant/orders/${id}`);
}
