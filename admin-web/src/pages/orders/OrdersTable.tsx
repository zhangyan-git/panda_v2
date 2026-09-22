import { ModalForm, PageContainer, ProFormTextArea, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, message, Tag, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { formatYuan } from '../../services/money';
import { listMiniappUsers } from '../../services/miniappUser';
import { cancelOrder, listOrders, type OrderQuery, type OrderSummary } from '../../services/order';
import { FULFILLMENT_STATUS, ORDER_SOURCE, ORDER_STATUS } from '../../services/orderLabels';
import { FULL_PAGE_PARAMS } from '../../services/pagination';
import { paymentMethodLabel } from '../../services/paymentMethodLabels';
import { requestErrorMessage } from '../../services/requestError';
import { listStores } from '../../services/store';
import CompositionTags from './CompositionTags';

/**
 * 订单列表，四个页面共用这一份：全部订单 + 按行类型分的三类（咖啡 / 幸运杯套 / 会员）。
 *
 * 为什么要分：V2 的订单是合并单（一杯饮品 + 若干加购品 + 一个会员套餐都在一单里），
 * 「这单算哪种订单」没有唯一答案。用户已定的口径是**按含哪类行归**：一张单同时含几类，
 * 就同时出现在几个列表里。所以三个分类列表之间不是互斥切分，各自的筛选条件只是把
 * 三个行类型标记其中之一钉成 true（后端 EXISTS 子查询，见 order_query.go）。
 *
 * 钉住的那一项就不再作为筛选项出现：一个「咖啡订单」页面上问「要不要有饮品行」，
 * 两个选项都只会得到同一批单。
 */

export type OrderCategory = 'all' | 'drink' | 'addon' | 'membership';

type LineTypeFilter = 'hasDrink' | 'hasAddon' | 'hasMembership';

const CATEGORY: Record<
  OrderCategory,
  { title: string; hint: string; pinned?: { field: LineTypeFilter; label: string } }
> = {
  all: {
    title: '全部订单',
    hint: '所有订单，不分类型。一张单同时买了咖啡、杯套和会员时，它在三个分类列表里各出现一次，这里是唯一一次。',
  },
  drink: {
    title: '咖啡订单',
    hint: '含饮品行的订单。纯会员订单不出现在这里——它没有要出杯的东西。',
    pinned: { field: 'hasDrink', label: '含饮品行' },
  },
  addon: {
    title: '幸运杯套订单',
    hint: '含加购行的订单。加购品就是幸运杯套一类的活动商品，每个还赠福卡。',
    pinned: { field: 'hasAddon', label: '含加购行' },
  },
  membership: {
    title: '会员订单',
    hint: '含会员行的订单。纯会员订单的履约状态是「无需履约」——会员是账号上的权益，不落在某台机器上。',
    pinned: { field: 'hasMembership', label: '含会员行' },
  },
};

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/**
 * 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。
 *
 * 后端 OrderFilter 里**没有 keyword**，orderNo / userId / storeId 全是 uuid 或单号上的等值
 * 比较。多一个尾空格换来的是「查不到」而不是模糊匹配——而调用方只会以为这个人没下过单。
 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/**
 * 「构成」那一列：这一单含哪几类行。渲染交给自己那一份 CompositionTags——订单详情的
 * 「订单构成」用的是同一个组件，同一张单在两处读起来是同一句话。
 */

export default function OrdersTable({ category }: { category: OrderCategory }) {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [storeNames, setStoreNames] = useState<Record<string, string>>({});
  const [cancelling, setCancelling] = useState<OrderSummary>();
  const { title, hint, pinned } = CATEGORY[category];

  /**
   * 手机号 → userId 的换算结果，按关键词缓存。
   *
   * 为什么要缓存：ProTable 的 request 在翻页、改每页条数、刷新时都会重跑，每次都去问一次
   * 用户列表等于给同一页数据多发几倍的请求，而「匹配到多个用户」的提示也会跟着弹好几遍。
   * 关键词没变就复用上一次的结果。只存最后一次：价值只在「同一次筛选的多次翻页」上。
   */
  const resolvedPhone = useRef<{ keyword: string; userId: string }>();

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listStores(FULL_PAGE_PARAMS);
        setStoreNames(Object.fromEntries(items.map((item) => [item.id, item.name])));
      } catch {
        // 忽略：门店列表要 admin:stores:view，只看订单的人不一定有。这一列取不到名称就退回
        // 显示 id（接口其实已经带了 storeName，这里主要是给下拉当候选）。
      }
    })();
  }, []);

  /**
   * 把「手机号」这一格换成 userId。
   *
   * 老后台支持按手机号搜订单，但 order 库只存 userId（用户数据在 identity 库，跨库），
   * 后端也**没有** keyword 参数。所以走两步：用现成的用户列表接口按关键词换出 userId，
   * 再拿 userId 去筛订单。0 条就当场说「查不到」——否则用户看到的是一个空列表，分不清是
   * 「这个人没下过单」还是「手机号打错了」。
   *
   * 这个输入框只在有 canViewMiniappUsers 时渲染，所以这里不做权限失败的分支：没权限的人
   * 根本填不出关键词。
   */
  const resolvePhone = async (keyword: string): Promise<string | undefined> => {
    const cached = resolvedPhone.current;
    if (cached?.keyword === keyword) return cached.userId;
    const { items } = await listMiniappUsers({ keyword, page: 1, pageSize: 5 });
    if (items.length === 0) {
      message.warning(`查不到手机号或昵称匹配「${keyword}」的用户`);
      return undefined;
    }
    if (items.length > 1) {
      // 关键词是「手机号前缀或昵称片段」，输入「138」或「咖啡」都会命中一批。取第一个并在
      // 提示里说清是谁，比静默挑一个（运营会以为是精确匹配）好。
      const first = items[0];
      message.info(
        `「${keyword}」匹配到 ${items.length} 个用户，按第一个筛：${first.phone || first.id}`,
      );
    }
    resolvedPhone.current = { keyword, userId: items[0].id };
    return items[0].id;
  };

  // 手机号那一格只对能看小程序用户的人渲染：换 userId 走的是另一个权限码的接口
  // （admin:miniapp-users:view），没有却渲染出来，填一次就是一次 403。
  const phoneSearchColumn: ProColumns<OrderSummary> = {
    title: '用户手机号',
    dataIndex: 'phone',
    hideInTable: true,
    fieldProps: { placeholder: '手机号前缀或昵称片段' },
  };

  /**
   * 「有没有某类行」的筛选格。
   *
   * 值只有两档，所以是下拉而不是开关：开关的「关」还兼着「不筛」的意思，分不清。
   * 被当前分类钉住的那一个不渲染——在「会员订单」页面上问「有没有会员行」，选哪个都是同一批单。
   */
  const lineTypeSearchColumn = (
    field: LineTypeFilter,
    label: string,
    texts: [string, string],
  ): ProColumns<OrderSummary> => ({
    title: label,
    dataIndex: field,
    valueType: 'select',
    hideInTable: true,
    valueEnum: {
      true: { text: texts[0] },
      false: { text: texts[1] },
    },
  });

  const columns: ProColumns<OrderSummary>[] = [
    ...(access.canViewMiniappUsers ? [phoneSearchColumn] : []),
    {
      // 仅搜索用的时间范围。用独立的 dataIndex，不和下面那列 createdAt 共用：同名的两列
      // 会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的时间列就空了。
      title: '创建时间',
      dataIndex: 'createdRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        // 后端只认带时区的 RFC3339（纯日期串没有时区，「今天」是业务时区的今天）。
        // transform 出来的两个键会并进 request 的 params。
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { createdFrom: toRFC3339(range[0]), createdTo: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '订单号',
      dataIndex: 'orderNo',
      copyable: true,
      ellipsis: true,
      width: 190,
      fieldProps: { placeholder: '完整订单号' },
    },
    {
      // 取杯号紧挨着订单号：客服接电话时手上要的就是这两个——「我的号是多少」是最高频的问题，
      // 让人点进详情才看得到不好用。取杯口屏幕上叫它取杯码，是同一个值，不是凭据。
      //
      // 150 是按内容留的：号本身是 12 位大写字母数字（去掉 I/O/0/1 的字母表），14px 下约 120px，
      // 加左右各 8px 内边距。给窄了这一格会溢出到隔壁列，表格真实宽度跟着涨、与 scroll.x 对不上。
      title: '取杯号',
      dataIndex: 'pickupCode',
      search: false,
      ellipsis: true,
      width: 150,
      render: (_, row) => dash(row.pickupCode),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(ORDER_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(ORDER_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '履约',
      dataIndex: 'fulfillmentStatus',
      search: false,
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(FULFILLMENT_STATUS, row.fulfillmentStatus);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '来源',
      dataIndex: 'source',
      valueType: 'select',
      valueEnum: searchOptions(ORDER_SOURCE),
      width: 90,
      render: (_, row) => enumMeta(ORDER_SOURCE, row.source).text,
    },
    {
      // 分类列表里这一列是「为什么这张单在这儿」的答案：合并单同时挂在几个列表里，
      // 只看列表名看不出它含别的什么。
      //
      // 200 是量出来的：三个标签（饮品/加购/会员套餐）排下来内容宽 176，而这种尺寸的单元格
      // 左右各 8px 内边距，所以要 ≥192 才装得下。给 170 时这一格的内容会溢出（实测
      // scrollWidth 184 > 自身 170）：溢出到隔壁列，表格的真实宽度也跟着涨，与 scroll.x
      // 声明的那个数对不上（见下面的 1610）。
      title: '构成',
      dataIndex: 'composition',
      search: false,
      width: 200,
      render: (_, row) => <CompositionTags row={row} />,
    },
    {
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 150,
      // 等值比较（uuid 列），给不出候选，所以是输入框。有 canViewMiniappUsers 的人可以改用
      // 上面那格手机号。
      fieldProps: { placeholder: '完整用户 ID' },
    },
    {
      title: '门店',
      dataIndex: 'storeId',
      valueType: 'select',
      // 搜索发出去的仍是 storeId。表格里优先用接口带的 storeName（它就是按 id 查出来的
      // 名字），没有才退回字典——门店被删过的历史单会缺名字。
      valueEnum: Object.fromEntries(
        Object.entries(storeNames).map(([id, name]) => [id, { text: name }]),
      ),
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.storeName || (row.storeId ? storeNames[row.storeId] : '')),
    },
    ...(pinned?.field === 'hasMembership'
      ? []
      : [lineTypeSearchColumn('hasMembership', '会员行', ['有会员行', '无会员行'])]),
    ...(pinned?.field === 'hasDrink'
      ? []
      : [lineTypeSearchColumn('hasDrink', '饮品行', ['有饮品行', '无饮品行'])]),
    ...(pinned?.field === 'hasAddon'
      ? []
      : [lineTypeSearchColumn('hasAddon', '加购行', ['有加购行', '无加购行'])]),
    {
      title: '实付',
      dataIndex: 'paidAmount',
      search: false,
      width: 100,
      render: (_, row) => formatYuan(row.paidAmount),
    },
    {
      title: '已退',
      dataIndex: 'refundedAmount',
      search: false,
      width: 100,
      render: (_, row) => formatYuan(row.refundedAmount),
    },
    {
      title: '支付方式',
      dataIndex: 'paymentMethod',
      search: false,
      width: 110,
      // 不按枚举映射：这是从支付事件里抄来的自由字符串，认不出来就原样显示。
      render: (_, row) => paymentMethodLabel(row.paymentMethod),
    },
    {
      // 这一单承诺赠送的福卡张数。售后审核要看它（>0 就得让人确认没抽过奖），列表里提前
      // 摆出来，审核的人不必先去订单详情翻一遍。
      //
      // 0 显示成「—」而不是「0 张」：福卡规则（方案 §3.1 的基础赠送 + 加购加赠）还没有任何
      // 服务拥有它，眼下几乎每单都是 0，一列 0 会让人以为「福卡功能没配」。与详情页
      // BasicTab 的「承诺福卡」同一口径。
      title: '承诺福卡',
      dataIndex: 'fortuneCardsExpected',
      search: false,
      width: 90,
      render: (_, row) => (row.fortuneCardsExpected > 0 ? `${row.fortuneCardsExpected} 张` : '—'),
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度：表格靠它算钉在右边的那一块，不给就由浏览器按内容分配。
      // scroll.x 也必须等于各列宽度之和（见下面的 1610）。
      //
      // 160 是量出来的：「查看详情」+「取消订单」两个 link 按钮并排内容宽 144（+ 左右各 8px
      // 内边距 = 160）。原先给 140，这一格的内容溢出，而**钉右列的溢出会直接把表格的
      // scrollWidth 顶大**（1560 → 1580），于是 scroll.x 声明的数与真实宽度对不上。
      // 这个缺陷原先藏着：当时列表里的单都已支付、只渲染一个按钮，带「待支付」的单
      // 在屏幕上它才露出来。
      width: 160,
      fixed: 'right',
      render: (_, row) => [
        <Button
          key="detail"
          type="link"
          size="small"
          onClick={() => history.push(`/orders/${row.id}`)}
        >
          查看详情
        </Button>,
        // 只有待支付能取消：后端 CancelOrder 的状态机只认 pending_payment -> cancelled，
        // 别的状态点了必然回 409。这里先把按钮藏掉，免得点出一个注定失败的弹窗。
        access.canCancelOrders && row.status === 'pending_payment' ? (
          <Button key="cancel" type="link" size="small" danger onClick={() => setCancelling(row)}>
            取消订单
          </Button>
        ) : null,
      ].filter(Boolean),
    },
  ];

  return (
    <PageContainer title={title} content={hint}>
      <ProTable<OrderSummary>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1850 = 190+150+90+90+90+200+150+160+100+100+110+90+170+160，各列 width 之和。
        // 钉右列必须有它，而且每一列的宽度都要装得下自己的内容（见上面「构成」「操作」
        // 「取杯号」三列的注释）。
        scroll={{ x: 1850 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const createdFrom = params.createdFrom;
          const createdTo = params.createdTo;
          // 逐字段挑，不整个透传：搜索表单里的 phone / createdRange 不是接口参数，透传过去
          // 只是给后端多几个它不认识的查询键。
          const query: OrderQuery = {
            page: params.current,
            pageSize: params.pageSize,
            orderNo: exact(params.orderNo),
            status: exact(params.status) as OrderQuery['status'],
            source: exact(params.source) as OrderQuery['source'],
            userId: exact(params.userId),
            storeId: exact(params.storeId),
            createdFrom: toRFC3339(createdFrom),
            createdTo: toRFC3339(createdTo),
          };
          // 三个行类型筛选要单独判一次才敢赋值：下拉没选时它是 undefined，直接写
          // `=== 'true'` 会得到 false，而 false 在接口那边是「只要没有这类行的单」——
          // 于是不筛这一项反而把带这类行的订单全筛掉了。
          (['hasDrink', 'hasAddon', 'hasMembership'] as const).forEach((field) => {
            if (field === pinned?.field) return;
            const raw = exact(params[field]);
            if (raw) query[field] = raw === 'true';
          });
          // 分类钉住的筛选放在最后写，且不参与上面的循环：这一条是页面的身份，
          // 无论搜索表单里出现过什么它都得赢。
          if (pinned) query[pinned.field] = true;
          const keyword = exact(params.phone);
          if (keyword) {
            const userId = await resolvePhone(keyword);
            // 换不出人就当场返回空页，不要退回「按原来的 userId 筛」——那样会把上一轮的结果
            // 当成这一轮的答案。
            if (!userId) return { data: [], total: 0, success: true };
            query.userId = userId;
          }
          const result = await listOrders(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/* 取消是人工干预（会写平台审计），所以文案里说清两件事：这一单会被关掉，以及这一下
          会被记下来。原因必填——后端空 reason 回 400，而且事后要能回答「当时为什么关了」。 */}
      <ModalForm<{ reason: string }>
        title={cancelling ? `取消订单 ${cancelling.orderNo}` : '取消订单'}
        open={!!cancelling}
        onOpenChange={(open) => {
          if (!open) setCancelling(undefined);
        }}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!cancelling) return true;
          try {
            await cancelOrder(cancelling.id, values.reason.trim());
          } catch (error) {
            message.error(requestErrorMessage(error, '取消失败，请稍后重试'));
            // 返回 false 让弹窗留在原地：原因已经填好了，不必让人重填一遍。
            return false;
          }
          message.success('已取消订单');
          actionRef.current?.reload();
          return true;
        }}
      >
        <Typography.Paragraph type="secondary">
          取消后这一单立刻变成「已取消」，用户不能再支付。这是人工干预：
          这一下会记进操作日志与订单状态流水，请把依据写清楚。
        </Typography.Paragraph>
        <ProFormTextArea
          name="reason"
          label="取消原因"
          placeholder="会写进订单状态流水，请说明依据（如：用户来电要求取消）"
          fieldProps={{ rows: 3 }}
          rules={[{ required: true, message: '请输入取消原因' }]}
        />
      </ModalForm>
    </PageContainer>
  );
}
