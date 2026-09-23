import { ModalForm, PageContainer, ProFormSelect, ProFormText, ProTable } from '@ant-design/pro-components';
import { Button, message, Space, Tag } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { createCouponType, listCouponTypes, updateCouponType, type CouponType } from '../../services/coupon';
import { useAccess } from '@umijs/max';
import { requestErrorMessage } from '../../services/requestError';
import { scrollableModalBody } from '../../components/common/modalProps';

export default function CouponTypesPage() {
  const access = useAccess(); const ref = useRef<ActionType>(); const [open, setOpen] = useState(false); const [editing, setEditing] = useState<CouponType>();
  const columns: ProColumns<CouponType>[] = [
    { title: '编码', dataIndex: 'code' }, { title: '名称', dataIndex: 'name' }, { title: '描述', dataIndex: 'description' },
    { title: '状态', dataIndex: 'status', render: (_, r) => <Tag color={r.status === 'active' ? 'green' : 'default'}>{r.status === 'active' ? '启用' : '停用'}</Tag> },
    { title: '操作', valueType: 'option', render: (_, r) => access.canWriteCouponTypes ? <Button type="link" onClick={() => { setEditing(r); setOpen(true); }}>编辑</Button> : null },
  ];
  return <PageContainer title="优惠券类型"><ProTable<CouponType> rowKey="id" actionRef={ref} columns={columns} search={false} request={async () => ({ data: await listCouponTypes(), success: true })} toolBarRender={() => access.canWriteCouponTypes ? [<Button key="add" type="primary" onClick={() => { setEditing(undefined); setOpen(true); }}>新建类型</Button>] : []} />
    {/* 表单只在首次挂载时读 initialValues，而 Modal 默认关闭时不卸载子节点。少了下面
        的 key/destroyOnClose，「编辑 A → 取消 → 编辑 B」表单里留着的还是 A 的字段值，
        点确定却按 editing.id 提交，把 A 的 name/description/status 写到 B 上。
        编码重复这一类只有后端判得了（code 上有唯一约束），失败时提示并返回 false，
        把弹窗留在原地，别让人重填一遍。 */}
    <ModalForm open={open} onOpenChange={setOpen} title={editing ? '编辑类型' : '新建类型'} initialValues={editing} key={editing?.id ?? 'new'} modalProps={{ ...scrollableModalBody, destroyOnClose: true }} onFinish={async (v) => { try { if (editing) { await updateCouponType(editing.id, v); } else { await createCouponType(v as Pick<CouponType, 'code' | 'name' | 'description' | 'status'>); } } catch (error) { message.error(requestErrorMessage(error, '保存失败，请稍后重试')); return false; } message.success('已保存'); ref.current?.reload(); return true; }}><ProFormText name="code" label="编码" disabled={!!editing} rules={[{ required: true }]} /><ProFormText name="name" label="名称" rules={[{ required: true }]} /><ProFormText name="description" label="描述" /><ProFormSelect name="status" label="状态" options={[{ label: '启用', value: 'active' }, { label: '停用', value: 'disabled' }]} /></ModalForm>
  </PageContainer>;
}
