import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Empty, Typography } from 'antd';
import { useState } from 'react';
import OperationLogDetailDrawer from '../../../../components/common/OperationLogDetailDrawer';
import OperationLogResultTag from '../../../../components/common/OperationLogResultTag';
import { actionText, adminUserText, moduleText } from '../../../../components/common/operationLogText';
import { listOperationLogs, type OperationLog } from '../../../../services/operationLog';
import { toPageParams } from '../../../../services/pagination';

/**
 * 设备详情 → 操作日志。这台设备身上发生过的每一次改动，最近的在最前。
 *
 * 筛选条件只有一项而且写死在这里（targetType=device + 本设备 id），所以没有搜索表单：
 * 在一台设备的详情里再筛「模块/操作人」是多余的，要那么查就去平台操作日志页。
 *
 * 走的是平台日志那一套（admin_operation_logs），不是本地审计表——审计只有一份。
 * 接口是服务端分页的，所以这里让 ProTable 自己翻页。
 */
export default function LogsTab({ deviceId }: { deviceId: string }) {
  const [detail, setDetail] = useState<OperationLog>();

  const columns: ProColumns<OperationLog>[] = [
    {
      title: '时间',
      dataIndex: 'occurredAt',
      valueType: 'dateTime',
      width: 170,
    },
    {
      title: '模块',
      dataIndex: 'module',
      width: 120,
      render: (_, row) => moduleText[row.module] ?? row.module,
    },
    {
      title: '动作',
      dataIndex: 'action',
      width: 110,
      render: (_, row) => actionText[row.action] ?? row.action,
    },
    {
      title: '操作描述',
      dataIndex: 'operation',
      ellipsis: true,
      render: (_, row) => row.operation || '—',
    },
    {
      title: '操作人',
      dataIndex: 'adminUsername',
      width: 150,
      render: (_, row) => adminUserText(row),
    },
    {
      title: '结果',
      dataIndex: 'result',
      width: 110,
      render: (_, row) => <OperationLogResultTag log={row} />,
    },
    {
      title: '操作',
      valueType: 'option',
      width: 80,
      render: (_, row) => [
        <a key="detail" onClick={() => setDetail(row)}>
          详情
        </a>,
      ],
    },
  ];

  return (
    <>
      <ProTable<OperationLog>
        rowKey="id"
        headerTitle="操作记录"
        columns={columns}
        search={false}
        scroll={{ x: 900 }}
        request={async (params) => {
          const result = await listOperationLogs({
            ...toPageParams(params),
            // 两个参数是「与」：后端按 (target_type, target_id) 建了索引，这是它当初
            // 建出来的用途。只给 targetId 会把别的类型里同一个 uuid 的行也捞进来
            // （本表对目标对象没有外键，那种撞车是可能的）。
            targetType: 'device',
            targetId: deviceId,
          });
          return { data: result.items, total: result.total, success: true };
        }}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={
                <span style={{ color: '#999' }}>
                  这台设备还没有操作记录。
                  <Typography.Text type="secondary">
                    {/* 审计事件先落各服务自己的 outbox，再由 relay 送过来，所以刚做完的
                        操作会晚一两秒出现。 */}
                    （新建、改名、上下架都会记在这里，事件送达有一两秒延迟）
                  </Typography.Text>
                </span>
              }
            />
          ),
        }}
      />

      <OperationLogDetailDrawer log={detail} onClose={() => setDetail(undefined)} />
    </>
  );
}
