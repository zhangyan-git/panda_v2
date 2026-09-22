import { ProDescriptions } from '@ant-design/pro-components';
import type { ProDescriptionsItemProps } from '@ant-design/pro-components';
import { Tag } from 'antd';
import type { Payment } from '../../../services/payment';
import { PAYMENT_STATUS } from '../../../services/paymentLabels';
import { dash, money, RawBlock } from './render';

/**
 * 支付单详情 → 基本信息。列的是**支付单主表上真有的**列。
 *
 * 三处刻意不映射：
 *
 * - `failureCode` / `failureMessage`：大小写与措辞都不统一（渠道来的 `USER_CANCELLED`
 *   与内部的 `insufficient_coffee_beans` 混着），查渠道文档、报障给渠道都靠原文。
 * - `requestId`：它是把这一次请求串起六张表的那根线，翻成中文就找不到东西了。
 * - `subject`：下单时写进来的商品描述，本来就是人话。
 *
 * 金额一律拼 ¥：这里是逐项摊开看的，一串光秃秃的数字会让人不确定哪个是钱（列表那边也拼，
 * 因为那一列的表头虽然写着「金额」，但同一行里还有别的数字）。
 */

export default function BasicTab({ payment }: { payment: Payment }) {
  const columns: ProDescriptionsItemProps<Payment>[] = [
    { title: '支付单号', dataIndex: 'paymentNo', copyable: true },
    { title: '订单号', dataIndex: 'orderNo', copyable: true },
    {
      title: '状态',
      dataIndex: 'status',
      render: (_, row) => {
        const meta = PAYMENT_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '金额', dataIndex: 'amount', render: (_, row) => money(row.amount) },
    // 从前这里还有一格「出资类型」，与下面的「支付方式」是两套词表。今天它们是同一个值
    // （payments.funding_type 那一列已经删掉，见 payment/012），所以只剩下面那一格。
    { title: '主体', dataIndex: 'subject', render: (_, row) => dash(row.subject) },
    { title: '用户 ID', dataIndex: 'userId', copyable: true },
    {
      // 渠道与方式都给名字优先、退回编码：纯账户出资（咖啡豆）没有渠道，两个都是空串。
      title: '渠道',
      dataIndex: 'channelName',
      render: (_, row) => dash(row.channelName || row.channelCode),
    },
    {
      title: '支付方式',
      dataIndex: 'methodName',
      render: (_, row) => dash(row.methodName || row.methodCode),
    },
    {
      title: '渠道交易号',
      dataIndex: 'providerTransactionId',
      copyable: true,
      render: (_, row) => dash(row.providerTransactionId),
    },
    { title: '失败码', dataIndex: 'failureCode', render: (_, row) => dash(row.failureCode) },
    {
      title: '失败原因',
      dataIndex: 'failureMessage',
      span: 2,
      render: (_, row) => dash(row.failureMessage),
    },
    { title: '请求 ID', dataIndex: 'requestId', copyable: true, render: (_, row) => dash(row.requestId) },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
    // 没发生过的时间是 null（不是零值），所以这几格空着是有意义的：expiresAt 空 = 这笔
    // 支付没有超时约束，paidAt 空 = 从没成功过。
    { title: '支付截止', dataIndex: 'expiresAt', valueType: 'dateTime' },
    { title: '支付时间', dataIndex: 'paidAt', valueType: 'dateTime' },
    { title: '关闭时间', dataIndex: 'closedAt', valueType: 'dateTime' },
    {
      // 非空 = 这笔钱确实动过账户域（纯豆支付扣豆的那笔账变）。对账时要拿它去
      // account-service 查那笔账。
      title: '账户账变 ID',
      dataIndex: 'accountEntryId',
      copyable: true,
      render: (_, row) => dash(row.accountEntryId),
    },
    { title: '账户出资时间', dataIndex: 'accountFundedAt', valueType: 'dateTime' },
    {
      // 老系统迁移过来的单号。本仓库自己的单都是 null——空着不代表数据缺了。
      title: '老系统 ID',
      dataIndex: 'legacyId',
      copyable: true,
      render: (_, row) => dash(row.legacyId),
    },
    {
      // 主键一起摆出来：拿到它才能在库里直接
      // SELECT * FROM payment_provider_calls WHERE payment_id = '…'。
      title: '支付单 ID',
      dataIndex: 'id',
      copyable: true,
    },
    // 渠道附加数据（小程序 openid、设备号一类）是 jsonb，形状随渠道变，原样贴出来。
    // 它不是给人扫的表格，是排查时核对的原始凭证。
    {
      title: '渠道附加数据',
      dataIndex: 'attach',
      span: 2,
      render: (_, row) => <RawBlock value={row.attach} maxHeight={200} />,
    },
  ];

  return (
    <ProDescriptions<Payment>
      column={2}
      dataSource={payment}
      title="支付单信息"
      columns={columns}
    />
  );
}
