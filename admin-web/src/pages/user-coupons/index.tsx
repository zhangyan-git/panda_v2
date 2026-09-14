import { ModalForm, PageContainer, ProDescriptions, ProFormTextArea, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { Button, Drawer, message, Tag } from 'antd';
import { useRef, useState } from 'react';
import { getUserCoupon, listUserCoupons, redeemUserCoupon, revokeUserCoupon, type UserCoupon, type UserCouponQuery } from '../../services/coupon';
import { useAccess } from '@umijs/max';

const statusOptions: Record<string, { text: string; color: string }> = {
  claimed: { text: '已领取', color: 'blue' },
  held: { text: '已预占', color: 'processing' },
  redeemed: { text: '已核销', color: 'success' },
  expired: { text: '已过期', color: 'default' },
  refunded: { text: '已退款', color: 'warning' },
  invalidated: { text: '已作废', color: 'error' },
};

const formatStatus = (status: string) => statusOptions[status] ?? { text: status, color: 'default' };

// 接口给的面值/最低消费是「分」的整数，列表和详情都按元展示。
// 与 pages/coupon-templates 的换算方向相反（那边是录入），但口径必须一致：
// 两边都除以 100，任何一处漏了都会让同一张券在两个页面上显示成不同的钱。
const formatYuan = (fen?: number) => (Number(fen ?? 0) / 100).toFixed(2);

// revoke 的语义是「作废未使用的券」（claimed/held -> invalidated），不是冲正已核销的券，
// 所以文案统一叫「作废」：叫「撤销」会让管理员以为能撤掉一笔已经核销的账。
type CouponAction = 'redeem' | 'revoke';
const actionTitle = (type: CouponAction) => (type === 'redeem' ? '核销用户券' : '作废用户券');

export default function UserCouponsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<UserCoupon>();
  const [actionTarget, setActionTarget] = useState<{ coupon: UserCoupon; type: CouponAction }>();

  const columns: ProColumns<UserCoupon>[] = [
    { title: '用户 ID', dataIndex: 'userId', copyable: true },
    { title: '用户券 ID', dataIndex: 'id', copyable: true, ellipsis: true },
    { title: '模板 ID', dataIndex: 'templateId', ellipsis: true },
    { title: '类型', dataIndex: 'couponTypeCode' },
    { title: '状态', dataIndex: 'status', valueType: 'select', valueEnum: Object.fromEntries(Object.entries(statusOptions).map(([value, item]) => [value, { text: item.text }])), render: (_, record) => { const item = formatStatus(record.status); return <Tag color={item.color}>{item.text}</Tag>; } },
    { title: '面值', dataIndex: 'faceValue', search: false, render: (_, record) => formatYuan(record.faceValue) },
    { title: '有效期', dataIndex: 'expiredAt', valueType: 'dateTime', search: false },
    { title: '操作', valueType: 'option', render: (_, record) => [
      <Button key="detail" type="link" onClick={async () => setDetail(await getUserCoupon(record.id))}>详情</Button>,
      access.canRedeemCoupon && record.status === 'claimed' ? <Button key="redeem" type="link" onClick={() => setActionTarget({ coupon: record, type: 'redeem' })}>核销</Button> : null,
      access.canRevokeCoupon && ['claimed', 'held'].includes(record.status) ? <Button key="revoke" type="link" danger onClick={() => setActionTarget({ coupon: record, type: 'revoke' })}>作废</Button> : null,
    ].filter(Boolean) },
  ];

  return <PageContainer title="用户券">
    <ProTable<UserCoupon> rowKey="id" actionRef={actionRef} columns={columns} search={{ labelWidth: 'auto' }} request={async (params) => {
      const query: UserCouponQuery = { page: params.current, pageSize: params.pageSize, userId: params.userId, status: params.status, batchId: params.batchId, templateId: params.templateId };
      const result = await listUserCoupons(query);
      return { data: result.items, total: result.total, success: true };
    }} />
    <Drawer title="用户券详情" width={640} open={!!detail} onClose={() => setDetail(undefined)}>
      {detail && <ProDescriptions<UserCoupon> column={2} dataSource={detail} columns={[
        { title: '用户券 ID', dataIndex: 'id' }, { title: '用户 ID', dataIndex: 'userId' }, { title: '模板 ID', dataIndex: 'templateId' }, { title: '批次 ID', dataIndex: 'batchId' }, { title: '券类型', dataIndex: 'couponTypeCode' }, { title: '领取方式', dataIndex: 'claimType' }, { title: '状态', dataIndex: 'status', render: (_, value) => { const item = formatStatus(String(value)); return <Tag color={item.color}>{item.text}</Tag>; } }, { title: '面值', dataIndex: 'faceValue', render: (_, record) => formatYuan(record.faceValue) }, { title: '最低消费', dataIndex: 'minPurchaseAmount', render: (_, record) => formatYuan(record.minPurchaseAmount) }, { title: '核销方式', dataIndex: 'redemptionType' }, { title: '生效时间', dataIndex: 'validFrom', valueType: 'dateTime' }, { title: '过期时间', dataIndex: 'expiredAt', valueType: 'dateTime' }, { title: '领取时间', dataIndex: 'claimedAt', valueType: 'dateTime' }, { title: '核销时间', dataIndex: 'redeemedAt', valueType: 'dateTime' }, { title: '作废时间', dataIndex: 'invalidatedAt', valueType: 'dateTime' }, { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' }, { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
      ]} />}
    </Drawer>
    {/* 用 ModalForm 而不是 Modal.confirm：命令式弹窗的 onOk 一旦 reject，
        antd 的 ActionButton 会把异常原样抛出去变成 unhandled rejection，
        dev 下直接糊一层错误浮层；返回 false 又会关掉弹窗、丢掉已填的原因。
        必填校验交给 rules，弹窗自然留在原地并显示行内错误。 */}
    <ModalForm<{ reason?: string }>
      open={!!actionTarget}
      onOpenChange={(next) => { if (!next) setActionTarget(undefined); }}
      title={actionTarget ? actionTitle(actionTarget.type) : ''}
      // initialValues 只在首次挂载生效，上一个券填的原因会留到下一个券：
      // 没有 initialValues 也仍然要 destroyOnClose，否则输入框内容不重置。
      modalProps={{ destroyOnClose: true }}
      onFinish={async (values) => {
        if (!actionTarget) return false;
        const { coupon, type } = actionTarget;
        const reason = values.reason?.trim();
        if (type === 'redeem') await redeemUserCoupon(coupon.id, { reason });
        else await revokeUserCoupon(coupon.id, { reason });
        message.success(`${actionTitle(type)}成功`);
        actionRef.current?.reload();
        if (detail?.id === coupon.id) setDetail(await getUserCoupon(coupon.id));
        return true;
      }}
    >
      <ProFormTextArea
        name="reason"
        label={actionTarget?.type === 'revoke' ? '作废原因' : '原因'}
        placeholder={actionTarget?.type === 'revoke' ? '请输入作废原因' : '请输入原因（可选）'}
        fieldProps={{ rows: 3 }}
        // 服务端不强制 revoke 要原因（UserCouponActionInput.reason 可选），
        // 这里要求必填是运营口径：作废是人工介入，得留下依据。
        rules={actionTarget?.type === 'revoke' ? [{ required: true, message: '请输入作废原因' }] : []}
      />
    </ModalForm>
  </PageContainer>;
}
