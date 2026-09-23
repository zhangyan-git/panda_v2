import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 支付域的接口。字段名与后端 dto（payment-service internal/dto/response.go 的 json tag）
 * 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处，否则 TypeScript 不报错，
 * 只是整列空白。
 *
 * 金额一律是「分」的整数（int64 到了 JS 里就是 number）。这一层不换算，页面按元展示。
 *
 * **这个文件里全是 GET。** 支付单是钱的既成事实，后面挂着出资行、记账流水、渠道调用、
 * 回调通知四张只增表——关单、改状态、重放回调、发起退款都要先有退款与对账的语义，那几件事
 * 本轮没做，后端那几条路径连路由都没注册，写出来只会得到一个 404。
 *
 * 收钱那一侧**在这个文件里已经没有东西了**：支付方式与渠道不再是库里的两行可写配置，而是
 * payment-service internal/catalog 里的三个常量（咖啡豆 / 银联商务小程序 / 银联商务 H5）。
 * 第四种「取货码」不在那里：它扣的是这台设备的咖啡余额，由 partner-service 验签后转
 * order-service 直接落单，压根不经过支付服务——所以支付域这边连它的名字都没有。
 * 运营没有可配的东西，后台也就没有可写的表单——原来那一页
 * （/payments/methods）连同它那几个写接口一起删了。
 */

// ——— 枚举 ———
// 取值来自 payment-service 的 model 常量（各表的 CHECK 约束），不是接口给的——接口回的
// 就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而这里没登记，界面上
// 就退回显示原始码。文案表在 services/paymentLabels.ts。

/** payments.status：支付单走到哪一步了 */
export type PaymentStatus =
  | 'created'
  | 'pending'
  | 'succeeded'
  | 'failed'
  | 'closed'
  | 'expired';

/** payment_fundings.status：一条出资行走到哪一步 */
export type PaymentFundingStatus = 'reserved' | 'succeeded' | 'failed' | 'released' | 'reversed';

/**
 * payment_methods.action：这一档方式怎么把钱收上来。
 *
 * 前端**认的是 action 不是 code**——加一档新方式（换个 code）只要 action 是认得的六个
 * 之一就不用改代码。所以这一列在配置页上必须显眼，它是这张表唯一的可执行语义。
 */
export type PaymentMethodAction =
  | 'jump_miniapp'
  | 'native_pay'
  | 'direct_pay'
  | 'qrcode'
  | 'h5'
  | 'account';

export type PaymentMethodStatus = 'enabled' | 'disabled';

/**
 * payment_channels.mode：沙箱还是真实渠道。
 *
 * 只有两个值，但它是这一页最要紧的一列：`live` 的渠道行连着真钱。
 */
export type PaymentChannelMode = 'sandbox' | 'live';

/** payment_channels.status：legacy_readonly = 老系统留下的只读渠道，能查不能新用 */
export type PaymentChannelStatus = 'enabled' | 'disabled' | 'legacy_readonly';

/** payment_state_transitions.actor_type：是谁做的这次迁移 */
export type PaymentActorType = 'user' | 'merchant' | 'admin' | 'system';

/** payment_state_transitions.aggregate_type：这条流水记的是谁的迁移 */
export type PaymentAggregateType =
  | 'payment'
  | 'funding'
  | 'refund'
  | 'agreement'
  | 'charge'
  | 'reconciliation';

/** payment_transactions.kind：这笔账是收、退还是冲正 */
export type PaymentTransactionKind = 'payment' | 'refund' | 'reversal';

/** payment_transactions.direction：钱进还是钱出 */
export type PaymentTransactionDirection = 'in' | 'out';

/** payment_provider_calls.operation：这次出网是在调渠道的哪件事 */
export type ProviderOperation =
  | 'create'
  | 'query'
  | 'close'
  | 'refund'
  | 'query_refund'
  | 'agreement_sign'
  | 'agreement_charge'
  | 'agreement_terminate'
  | 'reconcile';

/** payment_provider_calls.result：这次出网的结果。unknown = 没拿到明确答复 */
export type ProviderResult = 'success' | 'failed' | 'timeout' | 'unknown';

/** payment_notifications.status：一条回调通知处理到哪一步了 */
export type NotificationStatus = 'received' | 'processed' | 'ignored' | 'failed';

/**
 * 库里是 jsonb、后端原样透传的列，形状随业务变，所以是 unknown 而不是具体结构。
 *
 * 页面**不铺成一列一列**，原样 `<pre>` 贴出来：它们是排查时核对用的原始凭证
 * （渠道到底回了什么、这一档方式的启动参数是什么），不是给人扫的表格。
 */
type RawJSON = unknown;

// ——— 支付单 ———

/**
 * 一张支付单。**列表与详情是同一份形状**（后端也只有一个 dto），所以这里的类型两者共用，
 * 列定义抄过来也不会错位。
 */
export type Payment = {
  id: string;
  paymentNo: string;
  /** 老系统迁移过来的单号，没迁移过的是 null（本仓库自己的单都是 null）。 */
  legacyId: string | null;
  orderNo: string;
  userId: string;
  /** 分。 */
  amount: number;
  status: PaymentStatus;
  subject: string;
  /** 渠道附加数据（小程序 openid、设备号等），不放密钥。 */
  attach: RawJSON;
  providerTransactionId: string;
  /**
   * 失败码与失败话术。成功单上是**空串**而不是 null，而 closedAt 那类时间是**真 null**，
   * 两种都当空显示（页面上都是 `—`）。
   */
  failureCode: string;
  failureMessage: string;
  requestId: string;
  /**
   * 渠道与方式的 code 与中文名。两个 code 就在支付单自己那两列上（provider /
   * payment_method），名字由服务层照代码里的目录补上——**没有 uuid**：渠道与方式已经不是
   * 两张表了，没有什么可 JOIN 的。账户出资那条路上渠道是空串，于是 channelCode /
   * channelName 都是空，页面显示 `—`。
   */
  channelCode: string;
  channelName: string;
  methodCode: string;
  methodName: string;
  /** 非空 = 这笔钱确实动过账户域（纯豆支付扣豆的那笔账变）。 */
  accountEntryId: string | null;
  accountFundedAt: string | null;
  expiresAt: string | null;
  paidAt: string | null;
  closedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一条出资行：这笔钱按来源拆成的一行。
 *
 * lineType 就是**这笔出资的支付方式 code**，与同一张支付单上的 methodCode 是同一个值
 * （出资渠道那套词表已经退场，见 migrations/payment）。所以它是个自由字符串：合法的
 * 取值就是 catalog 里那几条，前端没有一份能对齐的枚举，中文名走 paymentMethodLabels。
 */
export type PaymentFunding = {
  id: string;
  lineNo: number;
  lineType: string;
  amount: number;
  status: PaymentFundingStatus;
  providerTransactionId: string;
  failureCode: string;
  accountEntryId: string | null;
  succeededAt: string | null;
  reversedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一条记账流水。这张表**只增不改**：一笔出资成功写一条 in，将来退款写一条 out，冲正再写
 * 一条。所以同一条出资行会有多行流水，靠 kind 与 direction 区分——页面上不合并。
 */
export type PaymentTransaction = {
  id: string;
  legacyId: string | null;
  kind: PaymentTransactionKind;
  paymentNo: string;
  refundNo: string;
  fundingLineNo: number;
  /** 这条流水对应的支付方式 code，与出资行 lineType 同一套值。见上面 PaymentFunding 的说明。 */
  lineType: string;
  direction: PaymentTransactionDirection;
  amount: number;
  /** 走的是哪家（ums / 空）。这一列是**文本**：渠道表已经删了，没有可指的 id。 */
  provider: string;
  providerTransactionId: string;
  accountEntryId: string | null;
  occurredAt: string;
  createdAt: string;
};

/**
 * 一条状态流转。
 *
 * `reason` 是**中英混着的诊断串**（`payment created` / `provider reported payment failed:
 * 用户取消` / `咖啡豆余额不足，需要 3400 分`），页面原样显示，不做码表映射——那种映射一定
 * 映射错，而它承载的恰恰是「当时到底发生了什么」这条唯一的一手信息。
 */
export type PaymentTransition = {
  id: string;
  aggregateType: PaymentAggregateType;
  aggregateId: string;
  fromStatus: string;
  toStatus: string;
  reason: string;
  requestId: string;
  actorType: PaymentActorType;
  actorId: string | null;
  metadata: RawJSON;
  createdAt: string;
};

/** 一次出网调用。请求/响应摘要原样显示（已由 provider 层做过脱敏，这里不再二次裁剪）。 */
export type PaymentProviderCall = {
  id: string;
  provider: string;
  operation: ProviderOperation;
  paymentNo: string;
  refundNo: string;
  agreementNo: string;
  requestId: string;
  traceId: string;
  /** 同一件事的第几次尝试。 */
  attemptNo: number;
  requestSummary: RawJSON;
  responseSummary: RawJSON;
  httpStatus: number | null;
  providerCode: string;
  providerMessage: string;
  result: ProviderResult;
  durationMs: number | null;
  createdAt: string;
};

/**
 * 一条回调通知。
 *
 * `body` 是渠道发来的原始报文。后端已经把它从 bytea 显式转成 string（不转的话 JSON 出的是
 * base64，页面上是乱码中的乱码），**非 UTF-8 的字节会被换成 U+FFFD**，所以页面上偶尔会
 * 看到替换字符——那不是解析错了。
 */
export type PaymentNotification = {
  id: string;
  provider: string;
  notificationId: string;
  eventType: string;
  paymentNo: string;
  refundNo: string;
  body: string;
  bodySha256: string;
  headers: RawJSON;
  signatureVerified: boolean;
  status: NotificationStatus;
  failureReason: string;
  receivedAt: string;
  processedAt: string | null;
};

/**
 * 支付单详情：一次把六个页签要的都给全。
 *
 * 六块在同一个只读事务里读出来（后端 repository.GetPaymentDetail），所以它们之间不会出现
 * 「支付单 succeeded 而出资行还停在 reserved」这种从未存在过的中间态。
 */
export type PaymentDetail = {
  payment: Payment;
  fundings: PaymentFunding[];
  transactions: PaymentTransaction[];
  transitions: PaymentTransition[];
  providerCalls: PaymentProviderCall[];
  notifications: PaymentNotification[];
};

// ——— 查询参数 ———

/**
 * 支付单筛选。
 *
 * paymentNo / orderNo 后端走 `ILIKE '%…%'`（**模糊**，与订单域的等值不同）：运维手里多半
 * 只有单号的一截。userId 是 uuid 列上的**等值**比较，且后端会先校验是不是合法 uuid——
 * 不是的话回 400 而不是让 Postgres 报类型错误。
 *
 * status / methodCode 走枚举白名单：打错一个字母回 400，不会静默返回空列表
 * （那会与「这段时间真的一张成功的单都没有」长得一模一样）。
 */
export type PaymentQuery = PageQuery & {
  paymentNo?: string;
  orderNo?: string;
  userId?: string;
  status?: PaymentStatus;
  /**
   * 按支付方式筛（`payments.payment_method`）。值域是 catalog 的 code——服务端拿那张目录
   * 校验，不是这里写死的联合类型：加一种支付方式只加 catalog 一条，别在这里补一次枚举。
   */
  methodCode?: string;
  /** 创建时间的闭开区间 [createdFrom, createdTo)，必须是带时区的 RFC3339（用 toRFC3339 转）。 */
  createdFrom?: string;
  createdTo?: string;
};

// ——— 接口 ———

export async function listPayments(params?: PaymentQuery) {
  return request<PageResult<Payment>>('/api/v1/admin/payments', { params });
}

/**
 * 支付单详情。路径参数是**支付单号**不是 uuid。
 *
 * 单号查无此单时后端回 404（`paymentNo` 是文本列，不像订单详情那样会因非 uuid 回 500），
 * 所以详情页拿到的是路由参数直接查，不必先校验形状。
 */
export async function getPaymentDetail(paymentNo: string) {
  return request<PaymentDetail>(`/api/v1/admin/payments/${paymentNo}`);
}
