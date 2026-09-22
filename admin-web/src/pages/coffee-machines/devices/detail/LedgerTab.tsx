import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Empty, Tag, Typography } from 'antd';
import { useEffect, useState } from 'react';
import {
  listDeviceBalanceEntries,
  type DeviceBalanceEntry,
} from '../../../../services/coffeeMachine';
import { listAdminUsers } from '../../../../services/iam';
import { formatSignedYuan, formatYuan } from '../../../../services/money';
import { FULL_PAGE_PARAMS, toPageParams } from '../../../../services/pagination';

/**
 * 设备详情 → 账变记录。这台设备的余额是怎么变成现在这样的。
 *
 * 这张表只增不改不删（库上有触发器挡着），所以它是**证据**而不是缓存：界面上一个写操作
 * 都没有，只有翻页。
 *
 * 金额是**有符号**的：正数加钱、负数扣钱，不是绝对值。显示时按符号上色，读的人一眼能
 * 分清哪几笔是把钱扣走的。变动前的余额库里没存（只存变动后），是后端用「变动后 - 金额」
 * 推出来的，前端不要再推第二次。
 */

const LEDGER_TYPE: Record<string, { label: string; color: string }> = {
  recharge: { label: '充值', color: 'green' },
  deduct: { label: '提货扣减', color: 'red' },
  adjust: { label: '余额调整', color: 'blue' },
  reverse: { label: '冲正', color: 'gold' },
};

/** 带符号的金额：加钱绿色带 +，扣钱红色带 -。**数字那部分是共享的**（services/money.ts）。 */
function signedYuan(fen: number) {
  const color = fen < 0 ? '#cf1322' : '#389e0d';
  return <span style={{ color }}>{formatSignedYuan(fen)}</span>;
}

export default function LedgerTab({ deviceId }: { deviceId: string }) {
  // 流水里的 operator_name 写入侧是**故意留空**的（令牌里没有用户名），展示名只能按
  // operator_id 现查。查不到就退回 id：空白比一串 uuid 更难排查。
  //
  // 用 FULL_PAGE_PARAMS 拉全集而不是按 id 逐个查：一次请求换一份字典，流水一页里
  // 通常也就一两个操作人。取不到不是错误（这一列只是给人看的），所以吞掉异常。
  const [adminNames, setAdminNames] = useState<Record<string, string>>({});

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listAdminUsers(FULL_PAGE_PARAMS);
        setAdminNames(Object.fromEntries(items.map((item) => [item.id, item.name || item.username])));
      } catch {
        // 忽略：管理员列表要 admin:users:view，看设备的人不一定有。
      }
    })();
  }, []);

  const columns: ProColumns<DeviceBalanceEntry>[] = [
    {
      title: '时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      width: 170,
    },
    {
      title: '类型',
      dataIndex: 'type',
      width: 110,
      render: (_, row) => {
        // 没登记的类型原样显示（`reverse` 这种词好过「未知」）：新类型上线时至少
        // 能看出它是什么，而不是一片「未知」。
        const meta = LEDGER_TYPE[row.type] ?? { label: row.type, color: 'default' };
        return <Tag color={meta.color}>{meta.label}</Tag>;
      },
    },
    {
      title: '金额',
      dataIndex: 'amount',
      width: 130,
      render: (_, row) => signedYuan(row.amount),
    },
    {
      title: '变动前',
      dataIndex: 'balanceBefore',
      width: 110,
      render: (_, row) => `¥${formatYuan(row.balanceBefore)}`,
    },
    {
      title: '变动后',
      dataIndex: 'balanceAfter',
      width: 110,
      render: (_, row) => `¥${formatYuan(row.balanceAfter)}`,
    },
    {
      // 「这笔钱是因为什么动的」。冲正流水指向被它冲掉的那一条，其余指向业务单据
      // （提货单、充值单）。参考对象的类型是各服务自己写的自由文本，不翻译。
      title: '关联',
      dataIndex: 'referenceId',
      ellipsis: true,
      render: (_, row) => {
        if (row.reversesEntryId) return `冲正 ${row.reversesEntryId.slice(0, 8)}`;
        if (!row.referenceType && !row.referenceId) return '—';
        return [row.referenceType, row.referenceId].filter(Boolean).join(' ');
      },
    },
    {
      title: '备注',
      dataIndex: 'remark',
      ellipsis: true,
      render: (_, row) => row.remark || '—',
    },
    {
      title: '操作人',
      dataIndex: 'operatorId',
      width: 150,
      render: (_, row) => {
        if (row.operatorName) return row.operatorName;
        if (!row.operatorId) return '—';
        return adminNames[row.operatorId] ?? row.operatorId;
      },
    },
  ];

  return (
    <ProTable<DeviceBalanceEntry>
      rowKey="id"
      headerTitle="余额流水"
      columns={columns}
      search={false}
      scroll={{ x: 1100 }}
      request={async (params) => {
        const result = await listDeviceBalanceEntries(deviceId, toPageParams(params));
        return { data: result.items, total: result.total, success: true };
      }}
      locale={{
        emptyText: (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description={
              <span style={{ color: '#999' }}>
                这台设备还没有余额流水。
                <Typography.Text type="secondary">
                  （小程序充值、提货扣减、后台调余额都会在这里留下一条，只增不改不删）
                </Typography.Text>
              </span>
            }
          />
        ),
      }}
    />
  );
}
