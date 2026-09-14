import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Tag } from 'antd';
import { listCouponBatches, type CouponBatch, type CouponBatchQuery } from '../../services/coupon';

const statusOptions = {
  pending: { text: '待处理', status: 'Default' },
  active: { text: '进行中', status: 'Processing' },
  exhausted: { text: '已耗尽', status: 'Success' },
  closed: { text: '已关闭', status: 'Error' },
};

const sourceOptions = {
  platform: '平台',
  admin: '后台',
  merchant: '商户',
  event: '活动',
  purchase: '购买',
};

export default function CouponBatchesPage() {
  const columns: ProColumns<CouponBatch>[] = [
    { title: '批次号', dataIndex: 'batchNo', copyable: true },
    { title: '模板 ID', dataIndex: 'templateId', ellipsis: true },
    {
      title: '来源',
      dataIndex: 'source',
      valueType: 'select',
      valueEnum: sourceOptions,
      render: (_, record) => sourceOptions[record.source as keyof typeof sourceOptions] ?? record.source,
    },
    { title: '总量', dataIndex: 'totalQuantity', search: false },
    { title: '预留', dataIndex: 'reservedQuantity', search: false },
    { title: '已发放', dataIndex: 'issuedQuantity', search: false },
    { title: '已释放', dataIndex: 'releasedQuantity', search: false },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: statusOptions,
      render: (_, record) => <Tag color={statusOptions[record.status as keyof typeof statusOptions]?.status}>{statusOptions[record.status as keyof typeof statusOptions]?.text ?? record.status}</Tag>,
    },
    { title: '创建人', dataIndex: 'createdBy', search: false, ellipsis: true },
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
