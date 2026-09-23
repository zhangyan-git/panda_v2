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
import { FULL_PAGE_PARAMS } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';
import { scrollableModalBody } from '../../components/common/modalProps';

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
                try {
                  await deletePermission(row.id);
                  message.success('已删除');
                  actionRef.current?.reload();
                } catch (error) {
                  // 还有角色绑着这条权限时后端会拒，理由只有后端知道；缺了这个 catch
                  // 界面上什么都不会发生，看起来像点了没反应。
                  message.error(requestErrorMessage(error, '删除失败，请稍后重试'));
                }
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
        request={async (params) => {
          // 这一页要全集：权限被聚合成「分组父行 + 权限子行」的树，只取第 1 页
          // 会把后面的分组整组漏掉，而表格看起来仍然正常。
          //
          // 筛选**在前端做**，理由与「要全集」是同一条：权限列表接口只认 page / pageSize
          // （user-service internal/handler/permission.go 的 List 只解析这两个），这一页
          // 又本来就要拉全量来聚合，没有一条「服务端筛」可接。以前这里干脆不接收 params，
          // 搜索框填了什么都没发生——那比没有搜索框更糟。四个框都是子串匹配、不区分大小写，
          // 空着的栏不参与过滤；筛完再聚合，所以不会留下空分组。
          const text = (value: unknown) => (typeof value === 'string' ? value.trim().toLowerCase() : '');
          const hit = (keyword: string, value?: string) =>
            !keyword || String(value ?? '').toLowerCase().includes(keyword);
          const code = text(params.code);
          const name = text(params.name);
          const group = text(params.group);
          const description = text(params.description);
          const perms = (await listPermissions(FULL_PAGE_PARAMS)).items.filter(
            (p) =>
              hit(code, p.code) &&
              hit(name, p.name) &&
              hit(group, p.group) &&
              hit(description, p.description),
          );
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
        // 表单只在首次挂载时读 initialValues，而 Modal 默认关闭时不卸载子节点。
        // 少了这两行，「编辑 A → 取消 → 编辑 B」表单里留着的还是 A 的字段值，
        // 点确定却按 editing.id 提交 —— 后端是全量覆盖，等于把 A 写到 B 上。
        key={editing?.id ?? 'new'}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
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
          try {
            if (editing) {
              await updatePermission(editing.id, values);
            } else {
              await createPermission(values);
            }
          } catch (error) {
            // 权限码重复一类只有后端判得了；返回 false 让弹窗留着，填过的四个字段不丢。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          message.success(editing ? '已更新' : '已创建');
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
