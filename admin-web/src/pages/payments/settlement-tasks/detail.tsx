import { PageContainer, ProDescriptions } from '@ant-design/pro-components';
import type { ProDescriptionsItemProps } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Alert, Card, Empty, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useRef, useState } from 'react';
import { formatDateTime } from '../../../services/datetime';
import { enumMeta } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';
import { requestErrorMessage } from '../../../services/requestError';
import {
  getSettlementTask,
  listSettlementChannels,
  type SettlementChannel,
  type SettlementReceiver,
  type SettlementTask,
  type SettlementTaskDetail,
} from '../../../services/settlement';
import {
  PARTY_TYPE,
  RECEIVER_STATUS,
  RECEIVER_TYPE,
  SCOPE_TYPE,
  TASK_STATUS,
  formatPercent,
  scopeNeedsRef,
} from '../../../services/settlementLabels';
import { EMPTY_SETTLEMENT_REFS, loadSettlementRefs, refName, type SettlementRefs } from '../settlementRefs';

/**
 * 分账明细详情：一条分账任务加上它的接收方明细。
 *
 * # 这一页最要紧的一件事是把恒等式摆出来
 *
 * **分账基数 = 平台自留 + 各接收方之和**。前两项在任务上、第三项要加起来才知道，所以接收方表
 * 底下有一个合计行，页头还有一条核对提示——三处对不上就是真出问题了（那意味着钱分了却没人拿到，
 * 或者分出去的钱比收进来的多）。
 *
 * # 接收方全是快照
 *
 * 比例、金额、主体名、渠道接收方号都是**建任务那一刻冻结下来的**（008 的设计），不回查账户与
 * 规则补当前值。所以账户后来改了名，这里显示的仍是当时那个名字——那不是数据旧了，那正是这条
 * 明细要回答的问题：当时分给了谁。
 *
 * # 时间字段
 *
 * `finishedAt` 只在分账成功后有值。008 的列注释写着：银联商务一次下发**没有「完结」那一步**，
 * 所以 `succeeded` 时就一并置上它——用一列而不是把状态劈成两半。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

/**
 * 可复制的值。
 *
 * ProTable 那一列上的 `copyable: true` 在这里用不了：这张表是**普通的 antd Table**（数据一次
 * 性给全，不走 ProTable 的请求生命周期），它的 ColumnsType 没有这个属性。两个号都需要复制
 * ——渠道接收方号要拿去跟渠道核对，明细号要拿去问渠道，手敲一个都不现实。
 */
const copyable = (value?: string | null) =>
  value ? <Typography.Text copyable>{value}</Typography.Text> : '—';

export default function SettlementTaskDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [detail, setDetail] = useState<SettlementTaskDetail>();
  const [loading, setLoading] = useState(true);
  // 取不到这一条时的原因。参数是 uuid，手敲一个不存在的 id 就是这个分支（后端回 404
  // ——它靠的是仓储把 pgx.ErrNoRows 翻成 ErrSettlementTaskNotFound，见那边的注释）。
  const [error, setError] = useState<string>();
  /** 引用字典，给范围与归属门店那两格的名字用。取不到就退回显示原始 id。 */
  const [refs, setRefs] = useState<SettlementRefs>(EMPTY_SETTLEMENT_REFS);
  /** 渠道目录，给「渠道」那一格的名字用（库里存的是 provider 码）。 */
  const [channels, setChannels] = useState<SettlementChannel[]>([]);

  /**
   * 请求序号，用来作废过期的那一次——见下面 load 的说明。
   */
  const loadSeq = useRef(0);

  /**
   * 取这一条分账任务的详情。三件事都要做，缺一个都会留下一种「看着像真的」的假象：
   *
   * - **进来先把上一条清掉**。不清的话，从一张明细跳到另一张的那一瞬间，页头、恒等式那条
   *   Alert 与接收方表显示的都还是**上一条**的内容——而那条 Alert 写的可能是「账是平的」。
   *   它会在几百毫秒后自己变成对的，但那几百毫秒里它是错的，而且看不出是在加载。
   * - **发出去的请求要能作废**（下面的 seq）：连续换 id 时，先发的那次可能后回来，落下来的
   *   就是**别人的**明细，而且它回来之后页面就不再变了——这次的错不是几百毫秒的事。
   * - **没有 id 时也要把 loading 停掉**：路由参数缺失时直接 return 会让页面一直转圈，
   *   看上去像服务挂了。
   */
  const load = useCallback(async () => {
    if (!id) {
      setDetail(undefined);
      setError('这一页的地址里没有分账任务 ID。');
      setLoading(false);
      return;
    }
    const seq = ++loadSeq.current;
    setDetail(undefined);
    setError(undefined);
    setLoading(true);
    try {
      const result = await getSettlementTask(id);
      if (seq !== loadSeq.current) return;
      setDetail(result);
    } catch (err) {
      if (seq !== loadSeq.current) return;
      setError(requestErrorMessage(err, '加载分账明细失败'));
    } finally {
      if (seq === loadSeq.current) setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    // 取不到就是空字典，下面各格退回显示原始 id。四份请求各自兜住了，这里的 catch 是兜住
    // 「映射里的 bug」那一种：真抛出来的话，也别在控制台留一个没人处理的 rejection。
    void loadSettlementRefs()
      .then(setRefs)
      .catch(() => setRefs(EMPTY_SETTLEMENT_REFS));
    // 渠道名取不到不该让这一页打不开：那一格退回显示 provider 码（`channelName` 的行为）。
    void listSettlementChannels()
      .then(setChannels)
      .catch(() => setChannels([]));
  }, []);

  /** provider 码 → 渠道名。取不到退回码本身。 */
  const channelName = (provider: string) =>
    channels.find((channel) => channel.provider === provider)?.name || provider;

  const task: SettlementTask | undefined = detail?.task;
  const receivers = detail?.receivers ?? [];

  /**
   * 接收方金额之和。
   *
   * 用整数分相加（接口给的就是整数分），**不经过浮点**：这里是拿三个数对账的地方，浮点尾数会
   * 让「差一分」看起来像正常。
   */
  const receiverTotal = receivers.reduce((sum, receiver) => sum + receiver.amount, 0);
  const balanced = !!task && task.baseAmount === task.platformAmount + receiverTotal;

  const taskColumns: ProDescriptionsItemProps<SettlementTask>[] = [
    { title: '分账任务号', dataIndex: 'taskNo', copyable: true, span: 2 },
    {
      title: '支付单号',
      dataIndex: 'paymentNo',
      copyable: true,
      render: (_, row) => (
        // 跳到支付单详情：那一页有出资行、渠道调用与回调报文，是这一笔钱的另一半。
        <Typography.Link onClick={() => history.push(`/payments/${row.paymentNo}`)}>
          {row.paymentNo}
        </Typography.Link>
      ),
    },
    { title: '订单号', dataIndex: 'orderNo', copyable: true, render: (_, row) => dash(row.orderNo) },
    {
      title: '状态',
      dataIndex: 'status',
      render: (_, row) => {
        const meta = enumMeta(TASK_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '分账基数', dataIndex: 'baseAmount', render: (_, row) => money(row.baseAmount) },
    {
      title: '平台自留',
      dataIndex: 'platformAmount',
      render: (_, row) => money(row.platformAmount),
    },
    {
      title: '接收方合计',
      dataIndex: 'receivers',
      // 这一格是从接收方那张表现加出来的，不是接口上的列。摆在这里是为了让恒等式的三项挨在一起。
      render: () => money(receiverTotal),
    },
    {
      // 空档位**不是异常**：没命中任何规则的那一笔就是这样（`settlement_tasks.scope_type`
      // 是 `DEFAULT ''`，与 scope_ref 一起表示「这笔没走规则」）。所以空的时候给个「—」而不是
      // 一枚写着一横的灰标签——标签是给「有个档位值、只是我没见过」留的。
      title: '范围档位',
      dataIndex: 'scopeType',
      render: (_, row) => {
        if (!row.scopeType) return '—';
        const meta = enumMeta(SCOPE_TYPE, row.scopeType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '范围',
      dataIndex: 'scopeRef',
      // **没有档位就既不是「全部」也不是某个范围**：`scope_type` 为空表示这一笔没走规则
      // （整单归平台），不是「规则管所有店铺」。与上面「范围档位」那一格同一个判断。
      render: (_, row) =>
        row.scopeType ? (scopeNeedsRef(row.scopeType) ? refName(refs, row.scopeRef) : '全部') : '—',
    },
    {
      // 空是常态：门店没配规则时这条任务照样建，整单归平台。
      title: '命中的规则',
      dataIndex: 'ruleName',
      render: (_, row) => dash(row.ruleName),
    },
    {
      title: '归属门店',
      dataIndex: 'storeRef',
      ellipsis: true,
      render: (_, row) => refName(refs, row.storeRef),
    },
    // 品牌与商户两列今天基本是空的（订单侧只推门店与设备），但只要有值就走与门店同一套：
    // 字典里认得出来就显示名字，认不出来就退回原始 id（`refName` 的行为）——原始 id 比一个
    // 查不到的「—」有用，而空白则一律是「—」。
    { title: '归属品牌', dataIndex: 'brandRef', render: (_, row) => refName(refs, row.brandRef) },
    { title: '归属商户', dataIndex: 'merchantRef', render: (_, row) => refName(refs, row.merchantRef) },
    {
      // 库里存的是 provider 码（今天是 `ums`），显示成渠道名——与「分账账户」那一页的同一列
      // 是同一种叫法。取不到目录就退回码，不留白。
      title: '渠道',
      dataIndex: 'provider',
      render: (_, row) => dash(row.provider && channelName(row.provider)),
    },
    {
      // 走仓库里那唯一一份支付方式文案（订单与支付单那两处读的也是它）。**不要在这里自己
      // 印码**：这一列存的就是 catalog 的 code，界面上一律显示中文名，与「支付单」那一页
      // 对同一笔钱的同一种叫法。
      title: '支付方式',
      dataIndex: 'method',
      render: (_, row) => paymentMethodLabel(row.method),
    },
    {
      title: '渠道分账号',
      dataIndex: 'providerTaskNo',
      copyable: true,
      render: (_, row) => dash(row.providerTaskNo),
    },
    {
      title: '渠道交易号',
      // 列名拼写跟的是后端的 JSON 标签（providerTransactionId），不是 Go 字段名。
      dataIndex: 'providerTransactionId',
      copyable: true,
      render: (_, row) => dash(row.providerTransactionId),
    },
    // 向渠道发起过几次。今天这条路是「随支付一次下发」，所以它最多是 1；
    // 将来有查单兜底时会更大。
    { title: '尝试次数', dataIndex: 'attempts' },
    {
      title: '最后错误',
      dataIndex: 'lastError',
      span: 2,
      render: (_, row) => dash(row.lastError),
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
    {
      // 只在分账成功后有值——见文件头的说明。
      title: '完成时间',
      dataIndex: 'finishedAt',
      valueType: 'dateTime',
    },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
    // 主键摆出来：拿到它才能在库里直接查这条任务与它的接收方。
    { title: '任务 ID', dataIndex: 'id', copyable: true },
  ];

  const receiverColumns: ColumnsType<SettlementReceiver> = [
    {
      title: '收款主体',
      dataIndex: 'partyType',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(PARTY_TYPE, row.partyType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // **快照**：账户后来改了名，这一格不变。那不是数据旧了，那正是这条明细要回答的问题。
      title: '主体名',
      dataIndex: 'partyName',
      ellipsis: true,
      width: 150,
      render: (_, row) => dash(row.partyName),
    },
    {
      // 当初实际发出去的那个号（子单的 mid）。
      title: '渠道接收方号',
      dataIndex: 'receiverId',
      ellipsis: true,
      width: 180,
      render: (_, row) => copyable(row.receiverId),
    },
    {
      title: '接收方类型',
      dataIndex: 'receiverType',
      width: 120,
      render: (_, row) => enumMeta(RECEIVER_TYPE, row.receiverType).text,
    },
    {
      title: '比例',
      dataIndex: 'ratioPercent',
      width: 90,
      align: 'right',
      // 固定额项记 0（008 的 CHECK 钉着），显示成 % 会让人以为它分不到。
      render: (_, row) => (row.ratioPercent > 0 ? formatPercent(row.ratioPercent) : '—'),
    },
    {
      title: '金额',
      dataIndex: 'amount',
      width: 110,
      align: 'right',
      render: (_, row) => money(row.amount),
    },
    {
      // 回退只在 settlement_reversals 那条路上发生，本刀不做，所以今天恒为 0。
      title: '已退回',
      dataIndex: 'reversedAmount',
      width: 110,
      align: 'right',
      render: (_, row) => (row.reversedAmount ? money(row.reversedAmount) : '—'),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(RECEIVER_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '渠道明细号',
      dataIndex: 'providerDetailNo',
      ellipsis: true,
      width: 170,
      render: (_, row) => copyable(row.providerDetailNo),
    },
    {
      title: '最后错误',
      dataIndex: 'lastError',
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.lastError),
    },
    { title: '创建时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
  ];

  return (
    <PageContainer
      loading={loading}
      title={task?.taskNo || '分账明细详情'}
      // 副标题放支付单号：从支付单那边跳过来的人手上拿的是它。
      subTitle={task?.paymentNo || undefined}
      onBack={() => history.push('/payments/settlement-tasks')}
    >
      {error ? (
        <Card>
          <Empty description={error} />
        </Card>
      ) : task ? (
        <>
          {/*
            恒等式核对。**这是这一页存在的主要理由**：分账基数在任务上、各接收方金额在下面那张表
            里，两项分开看都正常，只有加在一起才知道钱有没有全部分出去。
          */}
          <Alert
            type={balanced ? 'success' : 'error'}
            showIcon
            style={{ marginBottom: 16 }}
            message={
              balanced
                ? `账是平的：${money(task.platformAmount)}（平台自留）+ ${money(receiverTotal)}（${receivers.length} 个接收方）= ${money(task.baseAmount)}（分账基数）`
                : `账不平：平台自留 ${money(task.platformAmount)} + 接收方合计 ${money(receiverTotal)} ≠ 分账基数 ${money(task.baseAmount)}`
            }
            description={
              balanced
                ? '分出去的钱加上平台留下的，正好是这笔支付的实付金额。'
                : '这三项对不上意味着钱分了却没人拿到，或者分出去的钱比收进来的多——请连同支付单一起核对，并把这一条的 ID 报给开发。'
            }
          />
          <ProDescriptions<SettlementTask>
            column={2}
            dataSource={task}
            title="分账任务"
            columns={taskColumns}
          />
          <Card title={`接收方明细（${receivers.length}）`} style={{ marginTop: 16 }}>
            <Table<SettlementReceiver>
              rowKey="id"
              size="small"
              dataSource={receivers}
              // 1460 = 100+150+180+120+90+110+110+100+170+160+170，同 receiverColumns 各列
              // width 之和。
              scroll={{ x: 1460 }}
              pagination={false}
              columns={receiverColumns}
              locale={{
                // 空数组**不是错误**：没命中规则的那一笔就是这样——整单归平台，一个接收方都没有。
                emptyText: '这一笔没有接收方：整单都归平台（多半是这家门店没配规则）。',
              }}
              summary={() =>
                receivers.length ? (
                  <Table.Summary.Row>
                    <Table.Summary.Cell index={0} colSpan={5}>
                      接收方合计
                    </Table.Summary.Cell>
                    <Table.Summary.Cell index={5} align="right">
                      {money(receiverTotal)}
                    </Table.Summary.Cell>
                    <Table.Summary.Cell index={6} colSpan={5} />
                  </Table.Summary.Row>
                ) : null
              }
            />
          </Card>
        </>
      ) : null}
    </PageContainer>
  );
}
