import {
  CheckCircleOutlined,
  CloseCircleOutlined,
  DeleteOutlined,
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormDigit,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag, Tooltip } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  auditBrand,
  createBrand,
  deleteBrand,
  listBrands,
  updateBrand,
  updateBrandStatus,
  type AuditStatus,
  type Brand,
  type BrandInput,
  type BrandStatus,
} from '../../services/brand';
import { listMerchants } from '../../services/merchant';

const STATUS_TAG: Record<BrandStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '启用' },
  disabled: { color: 'default', label: '禁用' },
};

const AUDIT_TAG: Record<AuditStatus, { color: string; label: string }> = {
  pending: { color: 'gold', label: '待审核' },
  approved: { color: 'green', label: '已通过' },
  rejected: { color: 'red', label: '已驳回' },
};

const BrandsPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  // 新建/编辑 modal（editing 为 null 表示新建）
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Brand | null>(null);

  // 审核 modal（approve 决定通过/驳回）
  const [auditOpen, setAuditOpen] = useState(false);
  const [auditTarget, setAuditTarget] = useState<{ row: Brand; approve: boolean } | null>(null);

  const openAudit = (row: Brand, approve: boolean) => {
    setAuditTarget({ row, approve });
    setAuditOpen(true);
  };

  const merchantOptions = async () => {
    const data = await listMerchants();
    return data.map((m) => ({ label: m.name, value: m.id }));
  };

  const columns: ProColumns<Brand>[] = [
    { title: '品牌名称', dataIndex: 'name', width: 180, ellipsis: true },
    { title: '所属商户', dataIndex: 'merchantName', width: 160, ellipsis: true },
    {
      title: '状态',
      dataIndex: 'status',
      width: 80,
      render: (_, row) => {
        const tag = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
        return <Tag color={tag.color}>{tag.label}</Tag>;
      },
    },
    {
      title: '审核状态',
      dataIndex: 'auditStatus',
      width: 100,
      render: (_, row) => {
        const tag = AUDIT_TAG[row.auditStatus] ?? { color: 'default', label: row.auditStatus };
        const node = <Tag color={tag.color}>{tag.label}</Tag>;
        return row.auditRemark ? <Tooltip title={`审核备注：${row.auditRemark}`}>{node}</Tooltip> : node;
      },
    },
    {
      title: '可见',
      dataIndex: 'visible',
      width: 70,
      render: (_, r) => (r.visible ? <Tag color="blue">显示</Tag> : <Tag>隐藏</Tag>),
    },
    { title: '排序', dataIndex: 'sort', width: 70, search: false },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', width: 160 },
    {
      title: '操作',
      valueType: 'option',
      width: 320,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteBrands && row.auditStatus === 'pending' && (
            <>
              <Button
                type="link"
                size="small"
                icon={<CheckCircleOutlined />}
                onClick={() => openAudit(row, true)}
              >
                通过
              </Button>
              <Button
                type="link"
                size="small"
                danger
                icon={<CloseCircleOutlined />}
                onClick={() => openAudit(row, false)}
              >
                驳回
              </Button>
            </>
          )}
          {access.canWriteBrands && row.status === 'active' && (
            <Popconfirm
              title="禁用后该品牌在商户端不可用，确认禁用？"
              onConfirm={async () => {
                await updateBrandStatus(row.id, 'disabled');
                message.success('已禁用');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                禁用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteBrands && row.status === 'disabled' && (
            <Popconfirm
              title="确认启用该品牌？"
              onConfirm={async () => {
                await updateBrandStatus(row.id, 'active');
                message.success('已启用');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                启用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteBrands && (
            <Button
              type="link"
              size="small"
              onClick={() => {
                setEditing(row);
                setFormOpen(true);
              }}
            >
              编辑
            </Button>
          )}
          {access.canDeleteBrands && (
            <Popconfirm
              title="品牌下存在门店时无法删除，确认删除？"
              onConfirm={async () => {
                await deleteBrand(row.id);
                message.success('已删除');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" danger icon={<DeleteOutlined />}>
                删除
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="品牌管理">
      <ProTable<Brand>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1300 }}
        search={false}
        request={async () => {
          const data = await listBrands();
          return { data, success: true };
        }}
        toolBarRender={() => [
          access.canWriteBrands && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建品牌
            </Button>
          ),
        ]}
      />

      {/* 新建/编辑品牌 */}
      <ModalForm<BrandInput>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑品牌「${editing.name}」` : '新建品牌'}
        open={formOpen}
        onOpenChange={setFormOpen}
        modalProps={{ destroyOnClose: true }}
        initialValues={
          editing
            ? {
                merchantId: editing.merchantId,
                name: editing.name,
                logo: editing.logo,
                banner: editing.banner,
                description: editing.description,
                remark: editing.remark,
                visible: editing.visible,
                sort: editing.sort,
              }
            : { visible: true, sort: 0 }
        }
        onFinish={async (values) => {
          if (editing) {
            await updateBrand(editing.id, { ...values, merchantId: editing.merchantId });
            message.success('已保存，修改直接生效并留痕');
          } else {
            await createBrand(values);
            message.success('已创建，审核状态为待审核');
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="merchantId"
          label="所属商户"
          request={merchantOptions}
          disabled={!!editing}
          rules={[{ required: true, message: '请选择所属商户' }]}
        />
        <ProFormText
          name="name"
          label="品牌名称"
          rules={[{ required: true, message: '请输入品牌名称' }]}
        />
        <ProFormText name="logo" label="Logo URL" />
        <ProFormText name="banner" label="Banner URL" />
        <ProFormTextArea name="description" label="品牌描述" />
        <ProFormTextArea name="remark" label="备注" />
        <ProFormSwitch name="visible" label="小程序可见" />
        <ProFormDigit name="sort" label="排序" min={0} fieldProps={{ precision: 0 }} />
      </ModalForm>

      {/* 审核（通过/驳回） */}
      <ModalForm<{ remark?: string }>
        title={
          auditTarget?.approve
            ? `审核通过品牌「${auditTarget?.row.name ?? ''}」`
            : `驳回品牌「${auditTarget?.row.name ?? ''}」`
        }
        open={auditOpen}
        onOpenChange={setAuditOpen}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!auditTarget) return false;
          await auditBrand(auditTarget.row.id, auditTarget.approve, values.remark);
          message.success(auditTarget.approve ? '已通过' : '已驳回');
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormTextArea
          name="remark"
          label="审核备注"
          rules={auditTarget?.approve ? [] : [{ required: true, message: '驳回时必须填写原因' }]}
        />
      </ModalForm>
    </PageContainer>
  );
};

export default BrandsPage;
