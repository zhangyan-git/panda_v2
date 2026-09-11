import { AppstoreOutlined, DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Tag } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  createPermission,
  deletePermission,
  listPermissions,
  updatePermission,
  type Permission,
} from '../../services/iam';

type PermissionRow = Permission & { isGroup?: boolean; children?: PermissionRow[] };

const PermissionsPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [editing, setEditing] = useState<Permission | null>(null);
  const [modalOpen, setModalOpen] = useState(false);

  const columns: ProColumns<PermissionRow>[] = [
    {
      title: '权限码',
      dataIndex: 'code',
      width: 280,
      render: (_, row) =>
        row.isGroup ? (
          <span>
            <AppstoreOutlined style={{ marginRight: 8 }} />
            <span style={{ fontWeight: 600 }}>{row.group}</span>
            <span style={{ color: '#999', marginLeft: 8 }}>{row.children?.length ?? 0} 项</span>
          </span>
        ) : (
          <code style={{ fontSize: 12 }}>{row.code}</code>
        ),
    },
    {
      title: '名称',
      dataIndex: 'name',
      width: 160,
      render: (_, row) => (row.isGroup ? null : row.name),
    },
    {
      title: '分组',
      dataIndex: 'group',
      width: 140,
      render: (_, row) =>
        row.isGroup ? null : row.group ? (
          <Tag color="blue">{row.group}</Tag>
        ) : (
          <span style={{ color: '#999' }}>—</span>
        ),
    },
    { title: '说明', dataIndex: 'description', ellipsis: true },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      search: false,
      width: 180,
      render: (_, row) => (row.isGroup ? null : row.createdAt),
    },
    {
      title: '操作',
      valueType: 'option',
      width: 120,
      fixed: 'right',
      render: (_, row) => {
        if (row.isGroup) return null;
        return [
          access.canWritePermissions && (
            <Button
              key="edit"
              type="link"
              size="small"
              icon={<EditOutlined />}
              onClick={() => {
                setEditing(row);
                setModalOpen(true);
              }}
            >
              编辑
            </Button>
          ),
          access.canDeletePermissions && (
            <Popconfirm
              key="del"
              title="确认删除该权限？"
              onConfirm={async () => {
                await deletePermission(row.id);
                message.success('已删除');
                actionRef.current?.reload();
              }}
            >
              <Button type="link" size="small" danger icon={<DeleteOutlined />}>
                删除
              </Button>
            </Popconfirm>
          ),
        ];
      },
    },
  ];

  return (
    <PageContainer title="权限管理">
      <ProTable<PermissionRow>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        expandable={{ defaultExpandAllRows: true }}
        scroll={{ x: 1080 }}
        request={async () => {
          const perms = await listPermissions();
          // 按 group 聚合成树：分组为父行，权限为子行
          const grouped = new Map<string, Permission[]>();
          perms.forEach((p) => {
            const g = p.group || '其他';
            grouped.set(g, [...(grouped.get(g) ?? []), p]);
          });
          const data: PermissionRow[] = [...grouped.entries()].map(([group, children]) => ({
            id: `group:${group}`,
            code: '',
            name: '',
            description: '',
            group,
            createdAt: '',
            isGroup: true,
            children,
          }));
          return { data, success: true };
        }}
        search={{ labelWidth: 'auto' }}
        toolBarRender={() => [
          access.canWritePermissions && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setModalOpen(true);
              }}
            >
              新建权限
            </Button>
          ),
        ]}
      />

      <ModalForm<{ code: string; name: string; description?: string; group?: string }>
        title={editing ? '编辑权限' : '新建权限'}
        open={modalOpen}
        onOpenChange={(v) => {
          setModalOpen(v);
          if (!v) setEditing(null);
        }}
        initialValues={
          editing
            ? {
                code: editing.code,
                name: editing.name,
                description: editing.description,
                group: editing.group,
              }
            : undefined
        }
        onFinish={async (values) => {
          if (editing) {
            await updatePermission(editing.id, values);
            message.success('已更新');
          } else {
            await createPermission(values);
            message.success('已创建');
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="code"
          label="权限码"
          placeholder="如 admin:roles:view"
          rules={[{ required: true, message: '请输入权限码' }]}
        />
        <ProFormText
          name="name"
          label="名称"
          placeholder="如 查看角色"
          rules={[{ required: true, message: '请输入名称' }]}
        />
        <ProFormText name="group" label="分组" placeholder="如 角色管理" />
        <ProFormTextArea name="description" label="说明" fieldProps={{ rows: 2 }} />
      </ModalForm>
    </PageContainer>
  );
};

export default PermissionsPage;
