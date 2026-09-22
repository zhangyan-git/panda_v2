import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { history } from '@umijs/max';
import { message, Tag, Typography } from 'antd';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { formatYuan } from '../../services/money';
import { listPayments, type Payment, type PaymentQuery } from '../../services/payment';
import { PAYMENT_STATUS } from '../../services/paymentLabels';
import { PAYMENT_METHOD } from '../../services/paymentMethodLabels';
import { requestErrorMessage } from '../../services/requestError';

/**
 * 支付单列表。
 *
 * 这是一个**纯只读**的排查页：支付单是钱的既成事实，后面挂着出资行、记账流水、渠道调用、
 * 回调通知四张只增表，所以支付单这一棵树上一条写路由都没注册（关单、重放回调、发起退款都要
 * 先有退款与对账的语义，那两件事本轮没做）。这一页因此没有、也不该有操作按钮——除了「详情」，
 * 而那是另一个 GET。
 *
 * 支付方式与渠道**在后台已经没有可写的地方**：收钱那四种里有三种（咖啡豆 / 银联商务小程序 /
 * 银联商务 H5）写在 payment-service 的 internal/catalog 常量表里，密钥走环境变量；第四种
 * 取货码不走支付服务（扣的是设备余额，见 order-service 的 create_pickup.go），所以这一页
 * 上永远不会看到它。运营没有可配的东西，原来那一页连同它的写接口一起删了。这一页上的
 * methodName / channelName 仍然是中文——名字由服务端照代码里的目录补，不是查表得来的。
 *
 * 与订单列表的分工：那一页看的是「这一单买了什么」，这一页看的是「这笔钱是怎么收上来的」。
 * 两边都有支付单号，但订单域只存一份快照式的 payment_no，渠道、出资行、回调报文全在这边。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');
const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

/** 筛选项 trim 之后再发，空的整个丢掉——多一个尾空格换来的是「查不到」。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function PaymentsPage() {
  const columns: ProColumns<Payment>[] = [
    {
      // 仅搜索用的时间范围。**独立 dataIndex，不能与下面的 createdAt 同名**：同名的两列
      // 会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的时间列就空了
      // （后端收到数组则是直接 400）。这是仓库里踩过的坑。
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
      title: '支付单号',
      dataIndex: 'paymentNo',
      copyable: true,
      ellipsis: true,
      width: 240,
      // 后端是 ILIKE '%…%'，与订单号的等值比较不是一回事：运维手里多半只有单号的一截。
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      title: '订单号',
      dataIndex: 'orderNo',
      copyable: true,
      ellipsis: true,
      width: 250,
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      // 不 join 用户服务拿手机号：那是另一个权限码的接口，而且这一页是给对账的人看的，
      // 他手上拿的就是 userId。与会员列表同一条口径。
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 290,
      // 等值比较（uuid 列）。后端会先校验是不是合法 uuid：不是的话回 400，而不是让
      // Postgres 报类型错误——那会以 500 的面目弹在一个只是输错的搜索框上。
      fieldProps: { placeholder: '完整用户 ID（等值匹配）' },
    },
    {
      title: '金额',
      dataIndex: 'amount',
      search: false,
      width: 110,
      align: 'right',
      render: (_, row) => money(row.amount),
    },
    {
      // 渠道与方式都是**同库 JOIN** 出来的展示列（给运维看 uuid 没有意义）。渠道不给搜索：
      // 后端没有按渠道筛的参数，摆一个填了没用的下拉框比不摆更糟。
      title: '渠道',
      dataIndex: 'channelName',
      search: false,
      ellipsis: true,
      width: 130,
      // 纯账户出资（咖啡豆）没有渠道，两张表都是空串——那不是配坏了。
      render: (_, row) => dash(row.channelName || row.channelCode),
    },
    {
      // **出资类型那一列没有了**：它与支付方式是同一个值（payment_fundings.line_type 存的
      // 就是支付方式 code，见 payment/012），并排摆两列是同一件事说两遍；从前并排是因为它
      // 俩真是两套词表，而正因如此支付宝只能落成 other，这一列会显示成「其他」。
      //
      // dataIndex 用 methodCode 而不是 methodName：搜索框的键就是这个 dataIndex，而后端
      // 只认 code。名字优先、退回编码是**显示**口径，不是在过滤值。
      title: '支付方式',
      dataIndex: 'methodCode',
      valueType: 'select',
      valueEnum: searchOptions(PAYMENT_METHOD),
      ellipsis: true,
      width: 150,
      render: (_, row) => dash(row.methodName || row.methodCode),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(PAYMENT_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(PAYMENT_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    {
      // 没支付成功的单没有支付时间，接口给的是 null。
      title: '支付时间',
      dataIndex: 'paidAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // 100 与库存入库单那一列同宽：内容都是两个汉字的链接按钮，没必要各量一次。
      // fixed 的列必须显式给宽度——表格靠它算钉在右边的那一块，不给就由浏览器按内容分配，
      // 而钉右列的溢出会直接把表格的 scrollWidth 顶大、与 scroll.x 声明的数对不上。
      width: 100,
      fixed: 'right',
      render: (_, row) => [
        // 用 history.push 而不是 <a href>：这是个 SPA，<a> 会整页重载。仓库里列表页跳详情
        // 一律是这个写法。路径参数是**支付单号**不是 uuid。
        <Typography.Link key="view" onClick={() => history.push(`/payments/${row.paymentNo}`)}>
          详情
        </Typography.Link>,
      ],
    },
  ];

  return (
    <PageContainer
      title="支付单"
      content="每一笔支付的真实状态：出资行、记账流水、渠道调用与回调报文都挂在详情里。这一页只读——退款、关单、重放回调都还没有接口。"
    >
      <ProTable<Payment>
        rowKey="id"
        columns={columns}
        // 1710 = 240+250+290+110+130+150+100+170+170+100，各列 width 之和。
        // 钉右列必须有它，而且每一列的宽度都要装得下自己的内容（见上面「操作」那列的注释）。
        scroll={{ x: 1710 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          // 逐字段挑，不整个透传：搜索表单里的 createdRange 不是接口参数，透传过去只是给
          // 后端多几个它不认识的查询键。
          const query: PaymentQuery = {
            page: params.current,
            pageSize: params.pageSize,
            paymentNo: exact(params.paymentNo),
            orderNo: exact(params.orderNo),
            userId: exact(params.userId),
            status: exact(params.status) as PaymentQuery['status'],
            methodCode: exact(params.methodCode),
            createdFrom: params.createdFrom,
            createdTo: params.createdTo,
          };
          try {
            const result = await listPayments(query);
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            // 后端对不合法的筛选（不是 uuid 的 userId、打错的状态码）回 400，且带着一句
            // 能看懂的话。默不作声地显示空表会让人以为「这段时间真的一张单都没有」。
            message.error(requestErrorMessage(error, '加载支付单失败'));
            return { data: [], total: 0, success: false };
          }
        }}
      />
    </PageContainer>
  );
}
