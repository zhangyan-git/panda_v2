import { UserOutlined } from '@ant-design/icons';
import { ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { Avatar, Button, Drawer, Space, Tag, Tooltip, message } from 'antd';
import { useRef, useState } from 'react';
import { listMiniappUsers, type MiniappUser, type MiniappUserQuery } from '../../services/miniappUser';
import { toPageParams } from '../../services/pagination';

/**
 * 选用户：一个表单控件（值 = 选中的用户数组），点「选择用户」开抽屉，在抽屉里按
 * 关键词搜、翻页、勾选。
 *
 * 界面形态照老系统那张「选择用户」表格（关键词 + 表格 + 勾选），但**只有一个关键词
 * 输入框**，不是老系统那两栏「用户名 / 手机号」：V2 的 `/admin/miniapp-users` 只有
 * 一个 keyword 参数（手机号前缀或昵称片段），拆成两个框就是两个输入喂同一个参数，
 * 两边都填时还会让人以为是与关系。小程序用户页也是这一个框。
 *
 * 值里带上昵称与手机号是为了给人看（关不掉的标签要显示成谁），提交时只用 id。
 */

/** 已选中的一个用户。id 进提交载荷，另两样只用来显示。 */
export type PickedUser = {
  id: string;
  nickname: string;
  phone: string;
};

/**
 * 一次发放最多 1000 人。
 *
 * 这个数出自 coupon-service `internal/service/issue.go`：`len(req.UserIDs) > 1000`
 * 直接拒整批。在这里挡住是为了让人当场知道，而不是填完原因点确定才收到一句
 * 「参数错误」——那时已经看不出是人数超了。
 */
const MAX_PICKED = 1000;

/** 表单里最多铺几个标签，其余折成「+N」：选到几百人时标签墙会把弹窗顶穿。 */
const MAX_TAGS = 8;

const toPicked = (user: MiniappUser): PickedUser => ({
  id: user.id,
  nickname: user.nickname,
  phone: user.phone,
});

const pickedLabel = (user: PickedUser) => user.nickname || user.phone || user.id;

/**
 * 合并这次勾选变动的结果。
 *
 * ProTable 的 rowSelection.onChange 只回**这次变动涉及的行**，所以不能拿它当全集：
 * 先按 keys 把取消掉的删掉，再把这次带回来的补/更新进去。已经翻页离开的那些人不在
 * rows 里，但他们仍在 keys 里，靠上面那一步保住。
 */
function mergePicked(
  prev: Map<string, PickedUser>,
  keys: string[],
  rows: MiniappUser[],
): Map<string, PickedUser> {
  const next = new Map(prev);
  const kept = new Set(keys);
  for (const id of next.keys()) {
    if (!kept.has(id)) next.delete(id);
  }
  for (const row of rows) {
    if (kept.has(row.id)) next.set(row.id, toPicked(row));
  }
  if (next.size <= MAX_PICKED) return next;
  message.warning(`一次最多选 ${MAX_PICKED} 个用户，超出的没有加上`);
  // Map 保序，先勾的在前，所以截断等于「后来的不加」。
  return new Map([...next.entries()].slice(0, MAX_PICKED));
}

const columns: ProColumns<MiniappUser>[] = [
  {
    // 仅搜索用：后端按「手机号前缀 或 昵称片段」匹配，与小程序用户页同一套口径。
    title: '关键词',
    dataIndex: 'keyword',
    hideInTable: true,
    fieldProps: { placeholder: '手机号前缀或昵称' },
  },
  {
    title: '用户',
    dataIndex: 'nickname',
    search: false,
    width: 200,
    render: (_, row) => (
      <Space>
        <Avatar src={row.avatarUrl || undefined} icon={<UserOutlined />} size="small" />
        <span>{row.nickname || '（未设置昵称）'}</span>
      </Space>
    ),
  },
  {
    title: '手机号',
    dataIndex: 'phone',
    search: false,
    width: 140,
    render: (_, row) => row.phone || '—',
  },
  {
    // 列出来是为了别把券发给已注销的号：列表默认不筛状态，这些人会混在结果里。
    title: '状态',
    dataIndex: 'status',
    search: false,
    width: 90,
    valueEnum: {
      active: { text: '正常' },
      disabled: { text: '已禁用' },
      deleted: { text: '已注销' },
    },
  },
  {
    title: '注册时间',
    dataIndex: 'createdAt',
    valueType: 'dateTime',
    search: false,
    width: 170,
  },
];

export type UserPickerProps = {
  value?: PickedUser[];
  onChange?: (value: PickedUser[]) => void;
  disabled?: boolean;
};

export default function UserPicker({ value, onChange, disabled }: UserPickerProps) {
  const picked = value ?? [];
  const [open, setOpen] = useState(false);
  // 抽屉里的临时勾选：点确定才写回表单。点取消或直接关掉就当没选过，
  // 这与「打开抽屉不会改动表单」这件事一致。
  const [pending, setPending] = useState<Map<string, PickedUser>>(new Map());
  const actionRef = useRef<ActionType>();

  const openPicker = () => {
    setPending(new Map(picked.map((user) => [user.id, user])));
    setOpen(true);
  };

  const handleSelectionChange = (keys: string[], rows: MiniappUser[]) => {
    setPending((prev) => mergePicked(prev, keys, rows));
  };

  const remove = (id: string) => {
    onChange?.(picked.filter((user) => user.id !== id));
  };

  return (
    <>
      <Space direction="vertical" size={4} style={{ width: '100%' }}>
        <Space wrap size={[4, 4]}>
          {picked.slice(0, MAX_TAGS).map((user) => (
            // 标签上补一个手机号：昵称是用户自己起的，重名时一排「咖啡用户」看不出谁是谁。
            <Tooltip key={user.id} title={user.phone || user.id}>
              <Tag closable={!disabled} onClose={() => remove(user.id)}>
                {pickedLabel(user)}
              </Tag>
            </Tooltip>
          ))}
          {picked.length > MAX_TAGS && <Tag>+{picked.length - MAX_TAGS}</Tag>}
          {picked.length === 0 && <span style={{ color: '#999' }}>未选择用户</span>}
        </Space>
        <Space size={8}>
          <Button onClick={openPicker} disabled={disabled}>
            选择用户
          </Button>
          {picked.length > 0 && (
            <>
              <span style={{ color: '#666' }}>已选 {picked.length} 人</span>
              {!disabled && (
                <Button type="link" size="small" onClick={() => onChange?.([])}>
                  清空
                </Button>
              )}
            </>
          )}
        </Space>
      </Space>

      <Drawer
        title="选择用户"
        width={820}
        open={open}
        onClose={() => setOpen(false)}
        // 每次打开都重新挂载：不然上次的关键词、页码、表格缓存会跟着进来。
        destroyOnClose
        footer={
          <Space style={{ float: 'right' }}>
            <span style={{ color: '#666', marginRight: 8 }}>已选 {pending.size} 人</span>
            <Button onClick={() => setOpen(false)}>取消</Button>
            <Button
              type="primary"
              onClick={() => {
                onChange?.([...pending.values()]);
                setOpen(false);
              }}
            >
              确定
            </Button>
          </Space>
        }
      >
        <ProTable<MiniappUser>
          actionRef={actionRef}
          rowKey="id"
          columns={columns}
          search={{ labelWidth: 'auto' }}
          options={false}
          pagination={{ pageSize: 10, showSizeChanger: false }}
          rowSelection={{
            // 关掉它，翻页/改关键词时前面勾的人会在勾选表里被丢掉。
            preserveSelectedRowKeys: true,
            selectedRowKeys: [...pending.keys()],
            onChange: (keys, rows) => handleSelectionChange(keys as string[], rows),
          }}
          request={async (params) => {
            const result = await listMiniappUsers(toPageParams(params) as MiniappUserQuery);
            return { data: result.items, total: result.total, success: true };
          }}
          tableAlertRender={({ selectedRowKeys }) => (
            <Space size={16}>
              <span>已选 {selectedRowKeys.length} 人</span>
              {!disabled && (
                <a
                  onClick={() => {
                    setPending(new Map());
                    actionRef.current?.clearSelected?.();
                  }}
                >
                  清空已选
                </a>
              )}
            </Space>
          )}
        />
      </Drawer>
    </>
  );
}
