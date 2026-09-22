import { Alert, Card, Empty, Space, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useState } from 'react';
import { formatDateTime } from '../../../services/datetime';
import {
  getFortuneCardAccount,
  listFortuneCardEntries,
  listFortuneCardFreezes,
  type FortuneCardAccount,
  type FortuneCardEntry,
  type FortuneCardFreeze,
} from '../../../services/fortuneCard';
import {
  FORTUNE_CARD_ENTRY_TYPE,
  FORTUNE_CARD_FREEZE_STATUS,
  FORTUNE_CARD_REFERENCE_TYPE,
  fortuneCardAmountLabel,
} from '../../../services/fortuneCardLabels';
import type { OrderDetail } from '../../../services/order';
import { requestErrorMessage } from '../../../services/requestError';
import { pendingFortuneNotice } from './fortuneNotice';

/**
 * 订单详情 → 福卡：这一单送出去几张、到账没有。
 *
 * 数据来自**另一个服务另一个库**（account-service / panda_account），所以要单独取一次——
 * 订单详情那个响应里没有流水，只有「承诺了几张」（fortuneCardsExpected）。
 *
 * 这一屏回答两个不同的问题，两个都要摆：
 *   1. 这一单送了没有？→ 按 orderNo 查流水。有就是到账了。
 *   2. 这个人现在还有几张？→ 查账户余额。承诺 2 张、流水 +2，但余额 1，说明已经抽掉一张。
 * 只摆其中一个都会让人问出下一个问题（「到账了吗」/「那他现在有几张」）。
 *
 * 福卡是**抽奖凭证不是出资渠道**：这里不显示任何金额、也不参与订单的钱。列宽度按
 * 「张」算，别顺手套 money.ts 的那些格式化函数。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

export default function FortuneTab({ order }: { order: OrderDetail }) {
  const [entries, setEntries] = useState<FortuneCardEntry[]>([]);
  const [freezes, setFreezes] = useState<FortuneCardFreeze[]>([]);
  const [account, setAccount] = useState<FortuneCardAccount>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();

  /**
   * 售后单的「指纹」：申请、审核、撤销都会改 status，撤销之后连订单主状态都可能不动。
   * 用它与 updatedAt 拼一个字符串当依赖，是为了让这三个动作中的任何一个都能触发重取——
   * 直接依赖 order.afterSales 数组不行，那是每次渲染都新建的引用。
   */
  const afterSaleFingerprint = (order.afterSales ?? [])
    .map((item) => `${item.afterSaleNo}:${item.status}:${item.updatedAt}`)
    .join(',');

  /**
   * 依赖里带 status/finishedAt：订单在「标记完成」之后会被外面重新取一次，状态从 paid
   * 变成 completed——那一刻福卡才刚发出来，这一屏必须跟着再取一次。只依赖 orderNo 的话
   * 页面会停在没有流水的旧结果上，看起来像「完成了但没发福卡」。
   *
   * 也带 afterSale 那几项：申请/审核/撤销都会改冻结，而订单详情在售后动作之后会重新取
   * 一次订单——那一屏上的「冻结中 N 张」必须跟着变，否则客服会照着上一分钟的数字回答。
   */
  const load = useCallback(async () => {
    setLoading(true);
    try {
      // 一单的流水最多几条（基础一条 + 每个加购活动一条），200 是够用的整集口径，
      // 与 FULL_PAGE_PARAMS 同值。这里不用那个常量是因为它带 page 参数、而这一屏
      // 不翻页。
      const page = await listFortuneCardEntries({ orderNo: order.orderNo, pageSize: 200 });
      setEntries(page.items ?? []);
      // 冻结同样按订单取：绝大多数订单一条都没有，那一段就不渲染（见下）。
      const freezePage = await listFortuneCardFreezes({ orderNo: order.orderNo, pageSize: 200 });
      setFreezes(freezePage.items ?? []);
      // 余额单独取一次：接口是「按用户」而不是「按订单」，客服也常只要这一句。
      setAccount(await getFortuneCardAccount(order.userId));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载福卡流水失败'));
    } finally {
      setLoading(false);
    }
  }, [order.orderNo, order.userId, order.status, order.finishedAt, afterSaleFingerprint]);

  useEffect(() => {
    void load();
  }, [load]);

  const columns: ColumnsType<FortuneCardEntry> = [
    {
      title: '类型',
      dataIndex: 'entryType',
      width: 110,
      render: (_, row) => {
        const meta = FORTUNE_CARD_ENTRY_TYPE[row.entryType] ?? {
          text: row.entryType,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 带符号：这个数是有符号的，只显示绝对值会让一次抽奖看起来像又发了一张。
      title: '张数',
      dataIndex: 'amount',
      width: 80,
      render: (_, row) => fortuneCardAmountLabel(row.amount),
    },
    {
      // 变动**后**的余额，不是当前余额：冲正重放时只有它能回答「当时是多少」。
      title: '变动后余额',
      dataIndex: 'balanceAfter',
      width: 110,
    },
    { title: '说明', dataIndex: 'title', width: 260, ellipsis: true, render: (_, row) => dash(row.title) },
    {
      title: '来源',
      dataIndex: 'referenceType',
      width: 100,
      render: (_, row) => {
        const meta = FORTUNE_CARD_REFERENCE_TYPE[row.referenceType] ?? {
          text: row.referenceType,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 订单赠送时是订单号（也就是本单），冲正时是被冲的那条流水 id。
      title: '来源单号',
      dataIndex: 'referenceNo',
      width: 200,
      ellipsis: true,
      render: (_, row) => <Typography.Text copyable={!!row.referenceNo}>{dash(row.referenceNo)}</Typography.Text>,
    },
    {
      title: '被冲流水',
      dataIndex: 'reversesEntryId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.reversesEntryId),
    },
    { title: '记账时间', dataIndex: 'occurredAt', width: 170, render: (_, row) => time(row.occurredAt) },
    { title: '备注', dataIndex: 'remark', width: 200, ellipsis: true, render: (_, row) => dash(row.remark) },
  ];

  const freezeColumns: ColumnsType<FortuneCardFreeze> = [
    {
      // 客服拿这个号去售后页签对照那张申请单。
      title: '售后单号',
      dataIndex: 'afterSaleNo',
      width: 200,
      ellipsis: true,
      render: (_, row) => <Typography.Text copyable={!!row.afterSaleNo}>{dash(row.afterSaleNo)}</Typography.Text>,
    },
    {
      // 冻的是哪几笔发放。键的形状是 order:{id}:base / order:{id}:bonus:{campaignId}，
      // 「按退款范围冻」的语义全在这里——只退加购行时这一格只有一个 bonus 键。
      title: '冻结的发放',
      dataIndex: 'entryKeys',
      width: 320,
      ellipsis: true,
      render: (_, row) => (row.entryKeys?.length ? row.entryKeys.join('、') : '—'),
    },
    {
      // 这个数**不是**余额变化：冻结不改余额、不写流水，改的只是「这些张还能不能抽」。
      // 所以列名叫「冻结张数」而不是带符号的那个渲染。
      title: '冻结张数',
      dataIndex: 'amount',
      width: 100,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (_, row) => {
        const meta = FORTUNE_CARD_FREEZE_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '原因', dataIndex: 'reason', width: 200, ellipsis: true, render: (_, row) => dash(row.reason) },
    { title: '申请时间', dataIndex: 'occurredAt', width: 170, render: (_, row) => time(row.occurredAt) },
    {
      // 这一行**结束**的时刻。两种情况互斥（后端保证了只有一个非空），所以读哪一列由
      // 状态决定，而不是「先看 releasedAt 再看 recoveredAt」——后者会在两列都空时
      // 悄悄显示成「冻结中」，而冻结中本来就是空，那就分不清「还冻着」和「数据没到」了。
      title: '冻结结束时间',
      dataIndex: 'releasedAt',
      width: 170,
      render: (_, row) => time(row.status === 'recovered' ? row.recoveredAt : row.releasedAt),
    },
  ];

  // 只在「空着且本该有」时渲染，但先算出来——条件写在 JSX 里再算会让那一行长到读不懂。
  const notice = pendingFortuneNotice(order.status);
  const showPendingNotice =
    !loading && entries.length === 0 && order.fortuneCardsExpected > 0 && order.status !== 'completed';

  if (error) {
    return (
      <Card>
        <Empty description={error} />
      </Card>
    );
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={12}>
      <Space size={8} wrap>
        <Tag color={order.fortuneCardsExpected > 0 ? 'gold' : 'default'}>
          本单承诺赠送 {order.fortuneCardsExpected} 张
        </Tag>
        {account && (
          <Tag color={account.balance > 0 ? 'gold' : 'default'}>
            {account.hasAccount
              ? `该用户当前余额 ${account.balance} 张`
              : '该用户还没有福卡账户'}
          </Tag>
        )}
        {/*
          余额之外再给「可用 / 冻结」：用户问「我明明有 2 张，为什么抽奖说不够」时，
          答案就在这两个数上——有一张被退款申请冻着了。只给余额看不出这句话哪里错了。
        */}
        {account?.hasAccount && account.frozenBalance > 0 && (
          <Tag color="orange">
            可用 {account.availableBalance} 张（{account.frozenBalance} 张因退款申请冻结中）
          </Tag>
        )}
      </Space>

      {/*
        退款冻结：只有真有冻结行时才渲染——绝大多数订单一条都没有，一段永远空着的表格
        会让人以为「这一屏坏了」。也不加「没有冻结」的提示，因为「没有」是常态。
      */}
      {freezes.length > 0 && (
        <Card
          size="small"
          title="退款冻结"
          extra={
            <Typography.Text type="secondary">
              申请退款即冻结；驳回、用户撤销、退款失败后解冻，退款成功后追回（注销那几笔发放）
            </Typography.Text>
          }
        >
          <Table<FortuneCardFreeze>
            rowKey="id"
            size="small"
            loading={loading}
            columns={freezeColumns}
            dataSource={freezes}
            pagination={false}
            // 1270 = 200+320+100+110+200+170+170。没有钉右列，x 只用来兜住总宽。
            scroll={{ x: 1270 }}
          />
        </Card>
      )}

      {/*
        一句能解释「为什么这里空着」的话。福卡在**订单完成**时才发放，所以 paid 的单一
        条流水都没有是正常的，不是丢了。少了这句话，客服看到空白只会来问「是不是没发」。
        说法按状态分三种（见 pendingFortuneNotice）：只有 paid 才轮得到「去点标记完成」。
      */}
      {showPendingNotice && (
        <Alert type={notice.type} showIcon message={notice.message} description={notice.description} />
      )}

      <Table<FortuneCardEntry>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={entries}
        pagination={false}
        // 没有钉右列，x 只用来兜住总宽（1430 = 110+80+110+260+100+200+200+170+200）。
        // 这个数必须等于各列 width 之和：写小了表格不会横向滚动，宽列会被挤窄。
        scroll={{ x: 1430 }}
      />
    </Space>
  );
}
