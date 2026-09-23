import {
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormText,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  createManufacturer,
  listManufacturers,
  updateManufacturer,
  updateManufacturerStatus,
  type Manufacturer,
  type ManufacturerStatus,
} from '../../../services/coffeeMachine';
import { requestErrorMessage } from '../../../services/requestError';
import { scrollableModalBody } from '../../../components/common/modalProps';

const STATUS_TAG: Record<ManufacturerStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '启用' },
  disabled: { color: 'default', label: '停用' },
};

type ManufacturerFormValues = {
  code: string;
  name: string;
  contactName?: string;
  contactPhone?: string;
};

const ManufacturersPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Manufacturer | null>(null);

  const columns: ProColumns<Manufacturer>[] = [
    { title: '编码', dataIndex: 'code', width: 120 },
    { title: '名称', dataIndex: 'name', width: 200, ellipsis: true },
    {
      title: '联系人',
      dataIndex: 'contactName',
      width: 120,
      render: (_, row) => row.contactName || '—',
    },
    {
      title: '联系电话',
      dataIndex: 'contactPhone',
      width: 140,
      render: (_, row) => row.contactPhone || '—',
    },
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
      title: '操作',
      valueType: 'option',
      width: 160,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteCoffeeMachines && (
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
          {access.canWriteCoffeeMachines && row.status === 'active' && (
            <Popconfirm
              title="停用后该厂商下的设备不再同步，确认停用？"
              onConfirm={async () => {
                try {
                  await updateManufacturerStatus(row.id, 'disabled');
                  message.success('已停用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '停用失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                停用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteCoffeeMachines && row.status === 'disabled' && (
            <Popconfirm
              title="确认启用该厂商？"
              onConfirm={async () => {
                try {
                  await updateManufacturerStatus(row.id, 'active');
                  message.success('已启用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '启用失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                启用
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="厂商管理">
      <ProTable<Manufacturer>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 900 }}
        search={false}
        // 厂商是手动维护的个位数基础数据，接口也不分页——不翻页，一次全铺出来。
        pagination={false}
        request={async () => {
          const items = await listManufacturers();
          return { data: items, total: items.length, success: true };
        }}
        toolBarRender={() => [
          access.canWriteCoffeeMachines && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建厂商
            </Button>
          ),
        ]}
      />

      <ModalForm<ManufacturerFormValues>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑厂商「${editing.name}」` : '新建厂商'}
        open={formOpen}
        onOpenChange={setFormOpen}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
        initialValues={
          editing
            ? {
                code: editing.code,
                name: editing.name,
                contactName: editing.contactName,
                contactPhone: editing.contactPhone,
              }
            : {}
        }
        onFinish={async (values) => {
          try {
            if (editing) {
              // 只提交可改的那几列：编码不在编辑入参里，带了也不会被采纳。
              await updateManufacturer(editing.id, {
                name: values.name,
                contactName: values.contactName,
                contactPhone: values.contactPhone,
              });
              message.success('已保存');
            } else {
              await createManufacturer(values);
              message.success('已创建');
            }
          } catch (error) {
            // 撞编码唯一键回 409，后端的中文说明比「保存失败」有用得多。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="code"
          label="厂商编码"
          disabled={!!editing}
          tooltip={editing ? '编码创建后不可修改' : '设备与饮品同步时用来匹配厂商，创建后不可修改'}
          rules={[{ required: true, message: '请输入厂商编码' }]}
        />
        <ProFormText
          name="name"
          label="厂商名称"
          rules={[{ required: true, message: '请输入厂商名称' }]}
        />
        <ProFormText name="contactName" label="联系人" />
        <ProFormText name="contactPhone" label="联系电话" />
      </ModalForm>
    </PageContainer>
  );
};

export default ManufacturersPage;
