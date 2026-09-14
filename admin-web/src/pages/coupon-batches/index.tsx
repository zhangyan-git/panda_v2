import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Tag } from 'antd';
import { useEffect, useState } from 'react';
import { listCouponBatches, listCouponTemplates, type CouponBatch, type CouponBatchQuery } from '../../services/coupon';
import { BATCH_SOURCE, BATCH_STATUS } from '../../services/couponLabels';
import { enumMeta, searchOptions } from '../../services/labels';
import { listAdminUsers } from '../../services/iam';
import { FULL_PAGE_PARAMS } from '../../services/pagination';

export default function CouponBatchesPage() {
  // 这两列库里存的是 id（模板 id、发批次的账号 id），直接铺出来是一串 uuid，
  // 谁也认不出是哪个模板、哪个人。改成名称，取不到时退回显示 id——退回比空白好，
  // 至少还能拿去搜。
  const [templateNames, setTemplateNames] = useState<Record<string, string>>({});
  const [adminNames, setAdminNames] = useState<Record<string, string>>({});

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listCouponTemplates(FULL_PAGE_PARAMS);
        setTemplateNames(Object.fromEntries(items.map((item) => [item.id, item.name])));
      } catch {
        // 忽略：模板列表要 coupon:template:read 一类的权限，看批次的人不一定有。
        // 这里的映射只为了好看，取不到就显示 id，不该让整页报错。
      }
      try {
        const { items } = await listAdminUsers(FULL_PAGE_PARAMS);
        setAdminNames(Object.fromEntries(items.map((item) => [item.id, item.name || item.username])));
      } catch {
        // 同上：管理员列表要 users:read，缺了就退回 id。
      }
    })();
  }, []);

  const columns: ProColumns<CouponBatch>[] = [
    { title: '批次号', dataIndex: 'batchNo', copyable: true },
    {
      title: '模板',
      dataIndex: 'templateId',
      ellipsis: true,
      // valueEnum 只喂搜索下拉：给的是模板名，发出去的仍是 id（后端按 template_id 比）。
      valueType: 'select',
      valueEnum: Object.fromEntries(Object.entries(templateNames).map(([id, name]) => [id, { text: name }])),
      render: (_, record) => templateNames[record.templateId] ?? record.templateId,
    },
    {
      title: '来源',
      dataIndex: 'source',
      valueType: 'select',
      valueEnum: searchOptions(BATCH_SOURCE),
      render: (_, record) => enumMeta(BATCH_SOURCE, record.source).text,
    },
    { title: '总量', dataIndex: 'totalQuantity', search: false },
    { title: '预留', dataIndex: 'reservedQuantity', search: false },
    { title: '已发放', dataIndex: 'issuedQuantity', search: false },
    { title: '已释放', dataIndex: 'releasedQuantity', search: false },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(BATCH_STATUS),
      render: (_, record) => {
        const meta = enumMeta(BATCH_STATUS, record.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '创建人',
      dataIndex: 'createdBy',
      search: false,
      ellipsis: true,
      render: (_, record) => (record.createdBy ? (adminNames[record.createdBy] ?? record.createdBy) : '—'),
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime', search: false },
  ];

  return (
    <PageContainer title="发券批次">
      <ProTable<CouponBatch>
        rowKey="id"
        columns={columns}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: CouponBatchQuery = {
            page: params.current,
            pageSize: params.pageSize,
            templateId: params.templateId,
            status: params.status,
            source: params.source,
            batchNo: params.batchNo,
          };
          const result = await listCouponBatches(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />
    </PageContainer>
  );
}
