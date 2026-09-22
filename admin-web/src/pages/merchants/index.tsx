import {
  CheckCircleOutlined,
  DeleteOutlined,
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
  UserOutlined,
} from '@ant-design/icons';
import {
  DrawerForm,
  ModalForm,
  PageContainer,
  ProFormText,
  ProFormSelect,
  ProFormDependency,
  ProFormSwitch,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Switch, Tag, Select } from 'antd';
import { useRef, useState } from 'react';
import type { ActionType, ProColumns, ProFormInstance } from '@ant-design/pro-components';
import {
  createMerchant,
  createMerchantUser,
  deleteMerchant,
  deleteMerchantUser,
  listMerchantUsers,
  listMerchants,
  updateMerchant,
  updateMerchantStatus,
  updateMerchantUserScope,
  updateMerchantUserStatus,
  type Merchant,
  type MerchantStatus,
  type MerchantUser,
  type MerchantUserScopeType,
} from '../../services/merchant';
import { listBrands } from '../../services/brand';
import { listStores } from '../../services/store';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';

const STATUS_TAG: Record<MerchantStatus, { color: string; label: string }> = {
  pending: { color: 'gold', label: '待审核' },
  active: { color: 'green', label: '正常' },
  suspended: { color: 'red', label: '已暂停' },
};

const MerchantsPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  // 新建/编辑商户 modal（editing 为 null 表示新建）
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Merchant | null>(null);

  // 账号 drawer
  const [accountsOpen, setAccountsOpen] = useState(false);
  const [accountTarget, setAccountTarget] = useState<Merchant | null>(null);
  const [accountCreateOpen, setAccountCreateOpen] = useState(false);
  const [scopeOpen, setScopeOpen] = useState(false);
  const [scopeTarget, setScopeTarget] = useState<MerchantUser | null>(null);
  const accountTableRef = useRef<ActionType>();
  const accountCreateFormRef = useRef<ProFormInstance>();
  const scopeFormRef = useRef<ProFormInstance>();

  const openScope = (row: MerchantUser) => {
    setScopeTarget(row);
    setScopeOpen(true);
  };

  const openAccounts = (row: Merchant) => {
    setAccountTarget(row);
    setAccountsOpen(true);
  };

  const columns: ProColumns<Merchant>[] = [
    { title: '商户名称', dataIndex: 'name', width: 200, ellipsis: true },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      render: (_, row) => {
        const tag = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
        return <Tag color={tag.color}>{tag.label}</Tag>;
      },
    },
    { title: '联系人', dataIndex: 'contactName', width: 120, render: (_, r) => r.contactName || '—' },
    { title: '联系电话', dataIndex: 'contactPhone', width: 140, render: (_, r) => r.contactPhone || '—' },
    { title: '联系邮箱', dataIndex: 'contactEmail', width: 200, ellipsis: true, render: (_, r) => r.contactEmail || '—' },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      width: 160,
    },
    {
      title: '操作',
      valueType: 'option',
      // 四个按钮实测 302px，300 会顶破单元格右边缘（这一列钉在右边，溢出即出表格）。
      width: 320,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteMerchants && row.status === 'pending' && (
            <Popconfirm
              title="确认审核通过该商户？"
              onConfirm={async () => {
                try {
                  await updateMerchantStatus(row.id, 'active');
                  message.success('已审核通过');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '审核失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<CheckCircleOutlined />}>
                审核通过
              </Button>
            </Popconfirm>
          )}
          {access.canWriteMerchants && row.status === 'active' && (
            <Popconfirm
              title="暂停后该商户账号将无法登录，确认暂停？"
              onConfirm={async () => {
                try {
                  await updateMerchantStatus(row.id, 'suspended');
                  message.success('已暂停');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '暂停失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                暂停
              </Button>
            </Popconfirm>
          )}
          {access.canWriteMerchants && row.status === 'suspended' && (
            <Popconfirm
              title="确认恢复该商户？"
              onConfirm={async () => {
                try {
                  await updateMerchantStatus(row.id, 'active');
                  message.success('已恢复');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '恢复失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                恢复
              </Button>
            </Popconfirm>
          )}
          {access.canWriteMerchants && (
            <Button type="link" size="small" icon={<UserOutlined />} onClick={() => openAccounts(row)}>
              账号
            </Button>
          )}
          {access.canWriteMerchants && (
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
          {access.canDeleteMerchants && (
            <Popconfirm
              title="确认删除该商户？"
              onConfirm={async () => {
                try {
                  await deleteMerchant(row.id);
                  message.success('已删除');
                  actionRef.current?.reload();
                } catch (error) {
                  // 名下还有品牌/门店/账号时后端会拒，理由只有后端知道。
                  message.error(requestErrorMessage(error, '删除失败，请稍后重试'));
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

  const accountColumns: ProColumns<MerchantUser>[] = [
    { title: '用户名', dataIndex: 'username', copyable: true, width: 140 },
    { title: '姓名', dataIndex: 'name', width: 100, render: (_, r) => r.name || '—' },
    {
      title: '状态',
      dataIndex: 'status',
      width: 80,
      render: (_, r) =>
        r.status === 'active' ? <Tag color="success">启用</Tag> : <Tag color="default">禁用</Tag>,
    },
    {
      title: '数据范围',
      dataIndex: 'scopeType',
      width: 150,
      render: (_, row) =>
        `${row.scopeType === 'merchant' ? '商户全部' : row.scopeType === 'brand' ? '品牌' : '门店'}${row.scopeName ? `：${row.scopeName}` : ''}`,
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      width: 150,
    },
    {
      title: '操作',
      valueType: 'option',
      width: 220,
      render: (_, row) => (
        <Space>
          {row.status === 'active' ? (
            <Popconfirm title="禁用后该账号将无法登录，确认禁用？" onConfirm={async () => {
              try {
                await updateMerchantUserStatus(row.id, 'disabled');
                message.success('已禁用');
                accountTableRef.current?.reload();
              } catch (error) {
                message.error(requestErrorMessage(error, '禁用失败，请稍后重试'));
              }
            }}>
              <Button type="link" size="small" danger>禁用</Button>
            </Popconfirm>
          ) : (
            <Popconfirm title="确认启用该账号？" onConfirm={async () => {
              try {
                await updateMerchantUserStatus(row.id, 'active');
                message.success('已启用');
                accountTableRef.current?.reload();
              } catch (error) {
                message.error(requestErrorMessage(error, '启用失败，请稍后重试'));
              }
            }}>
              <Button type="link" size="small">启用</Button>
            </Popconfirm>
          )}
          <Button type="link" size="small" onClick={() => openScope(row)}>范围</Button>
          <Popconfirm title="确认删除该账号？" onConfirm={async () => {
            try {
              await deleteMerchantUser(row.id);
              message.success('已删除');
              accountTableRef.current?.reload();
            } catch (error) {
              message.error(requestErrorMessage(error, '删除失败，请稍后重试'));
            }
          }}>
            <Button type="link" size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="商户管理">
      <ProTable<Merchant>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // scroll.x 必须 ≥ 各列 width 之和（200+100+120+140+200+160+320=1240）：
        // 小于实际列宽之和时，钉在右边的操作列是按 scroll.x 定位的，会跟表体错开。
        scroll={{ x: 1240 }}
        search={false}
        request={async (params) => {
          const result = await listMerchants(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        toolBarRender={() => [
          access.canWriteMerchants && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建商户
            </Button>
          ),
        ]}
      />

      {/* 新建/编辑商户 */}
      <ModalForm<{
        name: string;
        contactName?: string;
        contactPhone?: string;
        contactEmail?: string;
      }>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑商户「${editing.name}」` : '新建商户'}
        open={formOpen}
        onOpenChange={setFormOpen}
        initialValues={
          editing
            ? {
                name: editing.name,
                contactName: editing.contactName,
                contactPhone: editing.contactPhone,
                contactEmail: editing.contactEmail,
              }
            : undefined
        }
        onFinish={async (values) => {
          try {
            if (editing) {
              await updateMerchant(editing.id, values);
            } else {
              await createMerchant(values);
            }
          } catch (error) {
            // 商户名重复一类只有后端判得了；返回 false 让弹窗留着，填过的字段不丢。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          message.success(editing ? '已保存' : '已创建，状态为待审核');
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="name"
          label="商户名称"
          rules={[{ required: true, message: '请输入商户名称' }]}
        />
        <ProFormText name="contactName" label="联系人" />
        <ProFormText name="contactPhone" label="联系电话" />
        <ProFormText name="contactEmail" label="联系邮箱" />
      </ModalForm>

      {/* 商户账号 */}
      <DrawerForm
        title={`「${accountTarget?.name ?? ''}」的登录账号`}
        open={accountsOpen}
        onOpenChange={(open) => {
          setAccountsOpen(open);
          if (!open) setAccountTarget(null);
        }}
        width={760}
        submitter={false}
      >
        <ProTable<MerchantUser>
          actionRef={accountTableRef}
          rowKey="id"
          columns={accountColumns}
          search={false}
          options={false}
          params={{ merchantId: accountTarget?.id }}
          request={async (params) => {
            if (!accountTarget) return { data: [], success: true };
            const result = await listMerchantUsers(accountTarget.id, toPageParams(params));
            return { data: result.items, total: result.total, success: true };
          }}
          toolBarRender={() => [
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => setAccountCreateOpen(true)}
            >
              新建账号
            </Button>,
          ]}
        />
      </DrawerForm>

      {/* 新建商户账号 */}
      <ModalForm<{
        username: string;
        password: string;
        name?: string;
        email?: string;
        phone?: string;
        isAdmin?: boolean;
        scopeType?: MerchantUserScopeType;
        scopeId?: string;
      }>
        title="新建登录账号"
        formRef={accountCreateFormRef}
        open={accountCreateOpen}
        onOpenChange={setAccountCreateOpen}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!accountTarget) return false;
          try {
            await createMerchantUser(accountTarget.id, values);
          } catch (error) {
            // 用户名全局唯一，重名只有后端知道；留着弹窗让人改一个再提交。
            message.error(requestErrorMessage(error, '账号创建失败，请稍后重试'));
            return false;
          }
          message.success('账号已创建');
          accountTableRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="username"
          label="用户名"
          extra="全局唯一，商户端用它登录"
          rules={[{ required: true, message: '请输入用户名' }]}
        />
        <ProFormText.Password
          name="password"
          label="初始密码"
          rules={[{ required: true, message: '请输入初始密码' }]}
        />
        <ProFormText name="name" label="姓名" rules={[{ required: true, message: '请输入姓名' }]} />
        <ProFormText name="email" label="邮箱" />
        <ProFormText name="phone" label="手机号" />
        <ProFormSwitch name="isAdmin" label="商户管理员" />
        <ProFormSelect
          name="scopeType"
          label="数据范围"
          initialValue="merchant"
          fieldProps={{
            onChange: () => {
              accountCreateFormRef.current?.setFieldsValue({ scopeId: undefined });
            },
          }}
          options={[
            { label: '商户全部数据', value: 'merchant' },
            { label: '单个品牌（旗下全部数据）', value: 'brand' },
            { label: '单个门店（旗下全部数据）', value: 'store' },
          ]}
        />
        <ProFormDependency name={['scopeType']}>
          {({ scopeType }) =>
            scopeType && scopeType !== 'merchant' ? (
              <ProFormSelect
                name="scopeId"
                label={scopeType === 'brand' ? '品牌' : '门店'}
                rules={[{ required: true, message: '请选择数据范围目标' }]}
                key={`${accountTarget?.id ?? 'none'}:${scopeType}`}
                params={{ merchantId: accountTarget?.id, scopeType }}
                request={async (params) => {
                  if (!params.merchantId) return [];
                  // 数据范围下拉要全集：分页后只给第 1 页会少掉可选项，
                  // 而且是静默的——用户只会发现「想选的品牌不在列表里」。
                  if (params.scopeType === 'brand') {
                    const brands = await listBrands({
                      ...FULL_PAGE_PARAMS,
                      merchantId: params.merchantId,
                    });
                    return brands.items.map((brand) => ({ label: brand.name, value: brand.id }));
                  }
                  const stores = await listStores({
                    ...FULL_PAGE_PARAMS,
                    merchantId: params.merchantId,
                  });
                  return stores.items.map((store) => ({ label: store.name, value: store.id }));
                }}
              />
            ) : null
          }
        </ProFormDependency>
      </ModalForm>

      <ModalForm<{ scopeType: MerchantUserScopeType; scopeId?: string; isAdmin?: boolean }>
        title={`调整账号「${scopeTarget?.username ?? ''}」的数据范围`}
        formRef={scopeFormRef}
        open={scopeOpen}
        onOpenChange={(open) => {
          setScopeOpen(open);
          if (!open) setScopeTarget(null);
        }}
        modalProps={{ destroyOnClose: true }}
        initialValues={scopeTarget ? { scopeType: scopeTarget.scopeType, scopeId: scopeTarget.scopeId, isAdmin: scopeTarget.isAdmin } : { scopeType: 'merchant' }}
        onFinish={async (values) => {
          if (!scopeTarget) return false;
          try {
            await updateMerchantUserScope(scopeTarget.id, values);
          } catch (error) {
            // 范围目标与所选层级对不上时后端会拒；留着弹窗让人重选，别静默失败。
            message.error(requestErrorMessage(error, '范围更新失败，请稍后重试'));
            return false;
          }
          message.success('数据范围已更新');
          accountTableRef.current?.reload();
          return true;
        }}
      >
        <ProFormSwitch name="isAdmin" label="商户管理员" />
        <ProFormSelect
          name="scopeType"
          label="数据范围"
          options={[
            { label: '商户全部数据', value: 'merchant' },
            { label: '单个品牌（旗下全部数据）', value: 'brand' },
            { label: '单个门店（旗下全部数据）', value: 'store' },
          ]}
          fieldProps={{
            onChange: () => {
              scopeFormRef.current?.setFieldsValue({ scopeId: undefined });
            },
          }}
        />
        <ProFormDependency name={['scopeType']}>
          {({ scopeType }) =>
            scopeType && scopeType !== 'merchant' ? (
              <ProFormSelect
                name="scopeId"
                label={scopeType === 'brand' ? '品牌' : '门店'}
                rules={[{ required: true, message: '请选择数据范围目标' }]}
                key={`${accountTarget?.id ?? 'none'}:${scopeType}`}
                params={{ merchantId: accountTarget?.id, scopeType }}
                request={async (params) => {
                  if (!params.merchantId) return [];
                  // 数据范围下拉要全集：分页后只给第 1 页会少掉可选项，
                  // 而且是静默的——用户只会发现「想选的品牌不在列表里」。
                  if (params.scopeType === 'brand') {
                    const brands = await listBrands({
                      ...FULL_PAGE_PARAMS,
                      merchantId: params.merchantId,
                    });
                    return brands.items.map((brand) => ({ label: brand.name, value: brand.id }));
                  }
                  const stores = await listStores({
                    ...FULL_PAGE_PARAMS,
                    merchantId: params.merchantId,
                  });
                  return stores.items.map((store) => ({ label: store.name, value: store.id }));
                }}
              />
            ) : null
          }
        </ProFormDependency>
      </ModalForm>
    </PageContainer>
  );
};

export default MerchantsPage;
