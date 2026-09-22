import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 订单域的接口。字段名与后端 dto（internal/dto/order.go、after_sale.go）的 json tag
 * 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处，否则 TypeScript 不报错，
 * 只是整列空白。
 *
 * 金额一律是「分」的整数（int64 到了 JS 里就是 number）。这一层不换算，页面按元展示。
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

/**
 * orders.source：这张单从哪儿来的。
 *
 * 四个取值与 order/005 + order/009 放宽后的 orders_source_check 逐字对应，也就是与
 * orderLabels 的 ORDER_SOURCE 表一一对应——改一处要同批改两处，否则筛选下拉里能选、
 * 类型上却传不出去（或者反过来：取值合法但列表上退回显示原始码）。
 */
export type OrderSource = 'miniapp' | 'screen_qr' | 'device' | 'renewal';
export type OrderLineType = 'drink' | 'addon' | 'membership';

/** order_payment_lines.status：一次出资分摊走到哪一步了 */
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

/** 退的范围。all=整单，其余三种是按行。 */
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
   * 自由字符串，不是枚举：它是从支付事件里原样抄下来的，后端没有对应的 CHECK。
   * 所以服务层不做映射，界面也按原文显示（见 paymentMethodLabel）。
   */
  paymentMethod: string;
  paymentNo: string;
  paidAt: string | null;
  finishedAt: string | null;
  cancelledAt: string | null;
  cancellationReason: string;
  expiresAt: string | null;
  remark: string;
  /**
   * 只读派生：这一单有没有饮品行 / 加购行 / 会员行，列表分类用。
   *
   * 三个都要摆出来（不能只显示「属于哪一类」）：一张合并单会同时出现在咖啡订单、幸运杯套订单、
   * 会员订单三个列表里，只看单看列表名没法解释「这张单为什么在这儿」，得让人一眼看出这单还含什么。
   */
  hasDrinkLine: boolean;
  hasAddonLine: boolean;
  hasMembershipLine: boolean;
  /** 下单时承诺赠送的福卡张数。审核退款申请时要看它（>0 就得让人确认没抽过奖）。 */
  fortuneCardsExpected: number;
  /**
   * 取杯号：这一单饮品行那个短号。没付成功或没有饮品行时为 null。
   *
   * 列表带上它是因为客服最常被问的就是「我的号是多少」；详情里同一份值也从行上给了一次。
   * 用户侧叫取杯号，取杯口屏幕上叫取杯码，是同一个值——不是凭据（见 order/004）。
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
  /** 取杯号，只有饮品行有，支付成功时生成。屏幕上人们也叫它取杯码，是同一个东西。 */
  pickupCode: string | null;
  remark: string;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一次出资分摊。
 *
 * 没有渠道流水号：`provider_transaction_id` 后端有意不映射（对账凭据，要看去
 * payment-service 查），所以这里既没有这个字段，页面也不该去找。
 *
 * `lineType` 是**支付方式的 code**，与这张订单上的 `paymentMethod` 是同一个值——出资渠道
 * 那套词表已经退场（migrations/order/008），所以它不再是一个能穷举的联合类型，中文名走
 * services/paymentMethodLabels。
 */
export type OrderPaymentLine = {
  id: string;
  lineNo: number;
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

// ——— 售后 ———

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
 * 售后单。申请书、用户撤销、后台审核、订单详情里挂的列表——四处都是同一个形状，
 * 后端只造了一个 view。后三个字段是只读派生（那三个数不在售后单上，是从订单和
 * 订单行上取的），审核福卡规则要看的就是它们。
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
  /** 支付域那张退款单的号；退款还没发起时是空串。 */
  refundNo: string;
  /** 退款失败码（渠道给的，如 ACQ.TRADE_NOT_EXIST）。只在 status='failed' 时有值。 */
  failureCode: string;
  /** 退款失败原因：渠道回的**原文**。码给机器认，这句话才是给人读的。与 failureCode 成对。 */
  failureMessage: string;
  reviewedBy: string | null;
  reviewedAt: string | null;
  reviewRemark: string;
  refundedAt: string | null;
  createdAt: string;
  updatedAt: string;
  /** 这一单承诺过的福卡张数与快照：>0 时审核通过必须先让人确认没抽过奖。 */
  fortuneCardsExpected: number;
  fortuneCardSnapshot: unknown;
  orderLine: AfterSaleLineRef | null;
};

// ——— 查询参数 ———

/**
 * 订单筛选。后端 OrderFilter 里**没有 keyword**：orderNo / userId / storeId / deviceId
 * 全是精确匹配（`=`），所以调用前要 trim，也不要指望它做模糊。
 */
export type OrderQuery = PageQuery & {
  orderNo?: string;
  userId?: string;
  status?: OrderStatus;
  source?: OrderSource;
  storeId?: string;
  deviceId?: string;
  /**
   * 按「有没有某类行」筛（后端是 EXISTS 子查询，不是主表上的列）。不传 = 不筛，与
   * `false`（要没有）是两回事，所以调用方只在真要筛的时候才赋值。
   *
   * 后端的「咖啡订单 / 幸运杯套订单 / 会员订单」三个列表分别钉 hasDrink / hasAddon /
   * hasMembership = true；一张合并单会在多个列表里出现，这是设计。
   */
  hasDrink?: boolean;
  hasAddon?: boolean;
  hasMembership?: boolean;
  /** 创建时间的闭开区间 [createdFrom, createdTo)，必须是带时区的 RFC3339（用 toRFC3339 转）。 */
  createdFrom?: string;
  createdTo?: string;
};

export type AfterSaleQuery = PageQuery & {
  afterSaleNo?: string;
  orderNo?: string;
  userId?: string;
  status?: AfterSaleStatus;
  scope?: AfterSaleScope;
  createdFrom?: string;
  createdTo?: string;
};

export type ReviewAfterSaleInput = {
  remark?: string;
  /**
   * 审核人对福卡规则的显式确认：本单赠送的福卡**没有参与过抽奖**。
   *
   * 为什么是人工勾：抽奖资格在 lottery-service（还没建），订单库只知道「承诺发几张」，
   * 判不了就不能静默放行——凡承诺过福卡的订单，没带这个确认后端一律拒绝
   * （FORTUNE_CARD_CONFIRMATION_REQUIRED）。
   */
  fortuneCardUnusedConfirmed?: boolean;
};

// ——— 接口 ———

export async function listOrders(params?: OrderQuery) {
  return request<PageResult<OrderSummary>>('/api/v1/admin/orders', { params });
}

/**
 * 订单详情。
 *
 * 后端对非 UUID 的 id 回的是 500 而不是 404（已知缺口，不在这里补）：路由把 id 直接交给
 * 查询，`where id = $1` 在 uuid 列上比字符串，Postgres 报类型错误。所以调用方要自己
 * 兜住——详情页拿到的是路由参数，用户手改地址栏是可能的。
 */
export async function getOrder(id: string) {
  return request<OrderDetail>(`/api/v1/admin/orders/${id}`);
}

/**
 * 后台取消订单。reason 必填（空串会 400），且取消人取自令牌——请求体里没有、也不该有
 * 「取消人」字段。
 *
 * 不带 Idempotency-Key：这条路径的语义是「把这一单关掉」，重复请求打到的是同一个状态机
 * 迁移，第二次会回 CONFLICT 而不是再关一次。
 */
export async function cancelOrder(id: string, reason: string) {
  return request<{ orderId: string; status: OrderStatus }>(
    `/api/v1/admin/orders/${id}/cancel`,
    { method: 'POST', data: { reason } },
  );
}

/**
 * 后台把订单标记为完成（paid → completed）。
 *
 * 不带请求体，也不像取消那样要理由：取消是**拒绝**一件事（要能回答「为什么关了」），完成是
 * **放行**（要求填理由只会催生一堆「无」）。这一下同样会写平台审计与订单状态流水，操作人
 * 取自令牌。
 *
 * 它有一个立刻发生的副作用：订单完成会发 `order.completed`，福卡账户域据此**真的给用户
 * 加福卡**（这一单承诺了几张就加几张）。所以这个按钮的确认文案要讲清楚，别做成「顺手点一下」。
 *
 * 不带 Idempotency-Key：同 cancelOrder，重复请求打到的是同一个状态机迁移，第二次回
 * CONFLICT。
 */
export async function completeOrder(id: string) {
  return request<{ orderId: string; status: OrderStatus }>(
    `/api/v1/admin/orders/${id}/complete`,
    { method: 'POST' },
  );
}

export async function listAfterSales(params?: AfterSaleQuery) {
  return request<PageResult<AfterSale>>('/api/v1/admin/after-sales', { params });
}

/**
 * 审核一张售后申请。动作写在路径上（approve / reject），不放进请求体：这样「审了什么」
 * 在访问日志里就看得见。
 *
 * approve 是**两步**：先把售后单记成「已通过」，紧接着向支付域发起退款。所以它能返回两种
 * 失败——审核本身没成（原样报错，单还是待审核），以及审核成了、发起退款没成（错误码
 * REFUND_NOT_STARTED，单停在「已通过」）。后者不该被当成「审核失败」处理：重试的入口是
 * 下面那个 startRefund，不是再点一次通过（那样只会拿到 409）。
 */
export async function reviewAfterSale(
  afterSaleNo: string,
  action: 'approve' | 'reject',
  data: ReviewAfterSaleInput = {},
) {
  return request<AfterSale>(`/api/v1/admin/after-sales/${afterSaleNo}/${action}`, {
    method: 'POST',
    data,
  });
}

/**
 * 把一张已通过、但退款没发起的售后单再推一次（重试出口）。
 *
 * 为什么不是「再点一次通过」：审核是一次决定，只该发生一次；发起退款是那次决定的执行，
 * 可以重试任意多次。后端两个动作也是两条路径、两个语义（见 routes/admin.go 那段注释）。
 *
 * 不带请求体：退多少、退哪一张支付单，全都记在售后单上了。重发是安全的——建退款单的幂等
 * 键就是售后单号，退过一次的单再推只会把同一张退款单拿回来。
 */
export async function startAfterSaleRefund(afterSaleNo: string) {
  return request<AfterSale>(`/api/v1/admin/after-sales/${afterSaleNo}/refund`, {
    method: 'POST',
  });
}
