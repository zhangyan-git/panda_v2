import { DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProFormTreeSelect,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { MENU_ICON_NAMES, renderMenuIcon } from '../../menuIcons';
import {
  createMenu,
  deleteMenu,
  listMenuTree,
  updateMenu,
  type MenuInput,
  type MenuNode,
} from '../../services/menu';

type MenuRow = MenuNode & { children?: MenuRow[] };

/** 把菜单树转成 TreeSelect 数据；editingId 子树会被禁用，防止选自己当父级 */
function toTreeSelectData(
  nodes: MenuNode[],
  editingId?: string,
): { title: string; value: string; disabled?: boolean; children?: any[] }[] {
  return nodes.map((n) => ({
    title: n.name,
    value: n.id,
    disabled: n.id === editingId,
    children: n.children?.length ? toTreeSelectData(n.children, editingId) : undefined,
  }));
}

const MenusPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<MenuNode | null>(null);
  const [treeData, setTreeData] = useState<MenuNode[]>([]);

  const openModal = (record?: MenuNode, parentId?: string) => {
    setEditing(record ?? null);
    setDefaultParent(parentId ?? '');
    setModalOpen(true);
  };

  const [defaultParent, setDefaultParent] = useState('');

  const columns: ProColumns<MenuRow>[] = [
    {
      title: '菜单名称',
      dataIndex: 'name',
      width: 260,
      render: (_, row) => (
        <Space>
          {renderMenuIcon(row.icon)}
          <span>{row.name}</span>
        </Space>
      ),
    },
    {
      title: '类型',
      dataIndex: 'path',
      search: false,
      width: 100,
      render: (_, row) =>
        row.path ? <Tag color="blue">菜单</Tag> : <Tag>目录</Tag>,
    },
    {
      title: '路由路径',
      dataIndex: 'path',
      search: false,
      width: 200,
      render: (_, row) =>
        row.path ? <code style={{ fontSize: 12 }}>{row.path}</code> : <span style={{ color: '#999' }}>—</span>,
    },
    {
      title: '图标',
      dataIndex: 'icon',
      search: false,
      width: 200,
      ellipsis: true,
      render: (_, row) => row.icon || <span style={{ color: '#999' }}>—</span>,
    },
    { title: '排序', dataIndex: 'sort', search: false, width: 80 },
    {
      title: '操作',
      valueType: 'option',
      width: 260,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteMenus && (
            <Button
              type="link"
              size="small"
              icon={<PlusOutlined />}
              onClick={() => openModal(undefined, row.id)}
            >
              新增子级
            </Button>
          )}
          {access.canWriteMenus && (
            <Button
              type="link"
              size="small"
              icon={<EditOutlined />}
              onClick={() => openModal(row)}
            >
              编辑
            </Button>
          )}
          {access.canDeleteMenus && (
            <Popconfirm
              title="确认删除该菜单？"
              onConfirm={async () => {
                await deleteMenu(row.id);
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
    <PageContainer title="菜单管理">
      <ProTable<MenuRow>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        pagination={false}
        expandable={{ defaultExpandAllRows: true }}
        scroll={{ x: 1080 }}
        request={async () => {
          const data = await listMenuTree();
          setTreeData(data);
          return { data: data as MenuRow[], success: true };
        }}
        search={false}
        toolBarRender={() => [
          access.canWriteMenus && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => openModal()}
            >
              新建菜单
            </Button>
          ),
        ]}
      />

      <ModalForm<MenuInput & { parentId?: string }>
        key={editing?.id ?? `new-${defaultParent}`}
        title={editing ? '编辑菜单' : '新建菜单'}
        open={modalOpen}
        onOpenChange={(v) => {
          setModalOpen(v);
          if (!v) setEditing(null);
        }}
        initialValues={
          editing
            ? {
                parentId: editing.parentId || undefined,
                name: editing.name,
                path: editing.path,
                icon: editing.icon || undefined,
                sort: editing.sort,
              }
            : { parentId: defaultParent || undefined, sort: 0 }
        }
        onFinish={async (values) => {
          const data: MenuInput = {
            parentId: values.parentId ?? '',
            name: values.name,
            path: values.path ?? '',
            icon: values.icon ?? '',
            sort: values.sort ?? 0,
          };
          if (editing) {
            await updateMenu(editing.id, data);
            message.success('已更新');
          } else {
            await createMenu(data);
            message.success('已创建');
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormTreeSelect
          name="parentId"
          label="父级菜单"
          placeholder="留空表示顶级"
          fieldProps={{
            treeData: toTreeSelectData(treeData, editing?.id),
            treeDefaultExpandAll: true,
            allowClear: true,
          }}
        />
        <ProFormText
          name="name"
          label="菜单名称"
          rules={[{ required: true, message: '请输入菜单名称' }]}
        />
        <ProFormText
          name="path"
          label="路由路径"
          placeholder="如 /roles；目录留空"
          extra="留空表示目录（仅分组，不可跳转）"
        />
        <ProFormSelect
          name="icon"
          label="图标"
          options={MENU_ICON_NAMES.map((name) => ({ label: name, value: name }))}
          fieldProps={{
            optionRender: (option) => (
              <Space>
                {renderMenuIcon(option.value as string)}
                {option.label}
              </Space>
            ),
            showSearch: true,
            allowClear: true,
          }}
        />
        <ProFormDigit name="sort" label="排序" min={0} fieldProps={{ precision: 0 }} />
      </ModalForm>
    </PageContainer>
  );
};

export default MenusPage;
