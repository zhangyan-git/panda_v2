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
  ProFormDependency,
  ProFormDigit,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { ProFormImageUpload, ProFormRegionCascader, namesToPath, regionFields } from '@panda-v2/ui';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag, Tooltip } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns, ProFormInstance } from '@ant-design/pro-components';
import { auditStore, createStore, deleteStore, listStores, updateStore, updateStoreStatus, type Store, type StoreInput, type StoreStatus } from '../../services/store';
import { listBrands } from '../../services/brand';
import { listMerchants } from '../../services/merchant';
import { deletionErrorMessage } from '../../services/requestError';
import { uploadImage } from '../../services/upload';

import type { AuditStatus } from '../../services/brand';

/**
 * 表单值 = 后端入参 + 级联框自己的编码路径。
 * `region` 只活在表单里，提交前由 regionFields 摊回三个区名与三个编码。
 */
type StoreFormValues = StoreInput & { region?: string[] };

const AUDIT_STATUS_TAG: Record<AuditStatus, { color: string; label: string }> = {
  pending: { color: 'gold', label: '待审核' },
  approved: { color: 'green', label: '已通过' },
  rejected: { color: 'red', label: '已驳回' },
};

const STATUS_TAG: Record<StoreStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '启用' },
  disabled: { color: 'default', label: '禁用' },
};

const StoresPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const formRef = useRef<ProFormInstance<StoreFormValues>>();

  // 新建/编辑 modal（editing 为 null 表示新建）
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Store | null>(null);

  // 审核 modal（approve 决定通过/驳回）
  const [auditOpen, setAuditOpen] = useState(false);
  const [auditTarget, setAuditTarget] = useState<{ row: Store; approve: boolean } | null>(null);

  const openAudit = (row: Store, approve: boolean) => {
    setAuditTarget({ row, approve });
    setAuditOpen(true);
  };

  const merchantOptions = async () => {
    const data = await listMerchants();
    return data.map((m) => ({ label: m.name, value: m.id }));
  };

  // 品牌级联：跟随已选商户刷新可选项
  const brandOptions = async (merchantId?: string) => {
    if (!merchantId) return [];
    const data = await listBrands({ merchantId });
    return data.map((b) => ({ label: b.name, value: b.id }));
  };

  const columns: ProColumns<Store>[] = [
    { title: '门店名称', dataIndex: 'name', width: 180, ellipsis: true },
    { title: '所属商户', dataIndex: 'merchantName', width: 150, ellipsis: true },
    { title: '所属品牌', dataIndex: 'brandName', width: 150, ellipsis: true },
    {
      title: '地址',
      dataIndex: 'address',
      width: 220,
      ellipsis: true,
      render: (_, r) =>
        [r.province, r.city, r.district, r.address].filter(Boolean).join('') || '—',
    },
    { title: '联系电话', dataIndex: 'phone', width: 130, render: (_, r) => r.phone || '—' },
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
        const tag = AUDIT_STATUS_TAG[row.auditStatus] ?? {
          color: 'default',
          label: row.auditStatus,
        };
        const node = <Tag color={tag.color}>{tag.label}</Tag>;
        return row.auditRemark ? (
          <Tooltip title={`审核备注：${row.auditRemark}`}>{node}</Tooltip>
        ) : (
          node
        );
      },
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', width: 160 },
    {
      title: '操作',
      valueType: 'option',
      width: 320,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteStores && row.auditStatus === 'pending' && (
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
          {access.canWriteStores && row.status === 'active' && (
            <Popconfirm
              title="禁用后该门店在商户端不可用，确认禁用？"
              onConfirm={async () => {
                await updateStoreStatus(row.id, 'disabled');
                message.success('已禁用');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                禁用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteStores && row.status === 'disabled' && (
            <Popconfirm
              title="确认启用该门店？"
              onConfirm={async () => {
                await updateStoreStatus(row.id, 'active');
                message.success('已启用');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                启用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteStores && (
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
          {access.canDeleteStores && (
            <Popconfirm
              title="指向该门店的账号范围将回收为商户级，确认删除？"
              onConfirm={async () => {
                try {
                  await deleteStore(row.id);
                  message.success('已删除');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(deletionErrorMessage(error));
                }
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
    <PageContainer title="门店管理">
      <ProTable<Store>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1500 }}
        search={false}
        request={async () => {
          const data = await listStores();
          return { data, success: true };
        }}
        toolBarRender={() => [
          access.canWriteStores && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建门店
            </Button>
          ),
        ]}
      />

      {/* 新建/编辑门店 */}
      <ModalForm<StoreFormValues>
        key={editing?.id ?? 'create'}
        formRef={formRef}
        onValuesChange={(changed) => {
          if ('merchantId' in changed) formRef.current?.setFieldValue('brandId', undefined);
        }}
        title={editing ? `编辑门店「${editing.name}」` : '新建门店'}
        open={formOpen}
        onOpenChange={setFormOpen}
        width={720}
        grid
        modalProps={{ destroyOnClose: true }}
        initialValues={
          editing
            ? {
                merchantId: editing.merchantId,
                brandId: editing.brandId,
                name: editing.name,
                logo: editing.logo,
                photos: editing.photos,
                // 名字换成编码路径。库里可能有「北京」这种自由文本，解析不出来就是
                // undefined（级联框空着），提交时再由 regionFields 原样兜回去。
                region: namesToPath([editing.province, editing.city, editing.district]),
                address: editing.address,
                longitude: editing.longitude ?? undefined,
                latitude: editing.latitude ?? undefined,
                phone: editing.phone,
                contactName: editing.contactName,
                contactPhone: editing.contactPhone,
                businessHours: editing.businessHours,
                detail: editing.detail,
                remark: editing.remark,
                visible: editing.visible,
              }
            : { visible: true }
        }
        onFinish={async (values) => {
          // 级联框只提交一个编码路径，三个区名与三个编码都在这里一次算出来。
          // 解析不出来（用户没碰过这个字段、而库里是历史自由文本）时，regionFields
          // 会把 editing 上的原值原样带回来——绝不能因为打开一次编辑就把老数据洗成空。
          const { region, ...rest } = values;
          const payload = { ...rest, ...regionFields(region, editing) };
          if (editing) {
            await updateStore(editing.id, { ...payload, merchantId: editing.merchantId });
            message.success('已保存，修改直接生效并留痕');
          } else {
            await createStore(payload);
            message.success('已创建，审核状态为待审核');
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="merchantId"
          label="所属商户"
          colProps={{ span: 12 }}
          request={merchantOptions}
          disabled={!!editing}
          fieldProps={{ showSearch: true, optionFilterProp: 'label' }}
          rules={[{ required: true, message: '请选择所属商户' }]}
        />
        <ProFormDependency name={['merchantId']}>
          {({ merchantId }) => (
            <ProFormSelect
              name="brandId"
              label="所属品牌"
              colProps={{ span: 12 }}
              key={merchantId ?? 'none'}
              params={{ merchantId }}
              request={(params) => brandOptions(params.merchantId)}
              disabled={!merchantId}
              fieldProps={{ showSearch: true, optionFilterProp: 'label' }}
              rules={[{ required: true, message: '请选择所属品牌' }]}
              extra="仅可选择该商户名下的品牌"
            />
          )}
        </ProFormDependency>
        <ProFormText
          name="name"
          label="门店名称"
          colProps={{ span: 12 }}
          rules={[{ required: true, message: '请输入门店名称' }]}
        />
        <ProFormImageUpload
          name="logo"
          label="Logo"
          colProps={{ span: 12 }}
          upload={uploadImage}
        />
        <ProFormRegionCascader name="region" label="省 / 市 / 区" colProps={{ span: 24 }} />
        <ProFormText name="address" label="详细地址" colProps={{ span: 24 }} />
        <ProFormDigit
          name="longitude"
          label="经度"
          colProps={{ span: 12 }}
          fieldProps={{ precision: 6 }}
        />
        <ProFormDigit
          name="latitude"
          label="纬度"
          colProps={{ span: 12 }}
          fieldProps={{ precision: 6 }}
        />
        <ProFormText name="phone" label="门店电话" colProps={{ span: 8 }} />
        <ProFormText name="contactName" label="联系人" colProps={{ span: 8 }} />
        <ProFormText name="contactPhone" label="联系人电话" colProps={{ span: 8 }} />
        <ProFormText
          name="businessHours"
          label="营业时间"
          colProps={{ span: 12 }}
          placeholder="如 09:00-22:00"
        />
        <ProFormImageUpload
          name="photos"
          label="门店照片"
          colProps={{ span: 24 }}
          maxCount={9}
          tooltip="上传门店照片，可多张"
          upload={uploadImage}
        />
        <ProFormTextArea name="detail" label="门店介绍" colProps={{ span: 24 }} />
        <ProFormTextArea name="remark" label="备注" colProps={{ span: 24 }} />
        <ProFormSwitch name="visible" label="小程序可见" colProps={{ span: 12 }} />
      </ModalForm>

      {/* 审核（通过/驳回） */}
      <ModalForm<{ remark?: string }>
        title={
          auditTarget?.approve
            ? `审核通过门店「${auditTarget?.row.name ?? ''}」`
            : `驳回门店「${auditTarget?.row.name ?? ''}」`
        }
        open={auditOpen}
        onOpenChange={setAuditOpen}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!auditTarget) return false;
          await auditStore(auditTarget.row.id, auditTarget.approve, values.remark);
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

export default StoresPage;
