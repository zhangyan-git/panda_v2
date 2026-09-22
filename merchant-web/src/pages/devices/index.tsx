import { PageContainer, ProTable } from '@ant-design/pro-components';
import { history } from '@umijs/max';
import { Alert, Button, Tag } from 'antd';
import { useEffect, useState } from 'react';
import type { ProColumns } from '@ant-design/pro-components';
import { listDevices, type DeviceSummary } from '../../services/coffeeMachine';
import { DEVICE_STATUS, vendorOnlineMeta } from '../../services/coffeeMachineLabels';
import { enumMeta, searchOptions } from '../../services/labels';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { listStores } from '../../services/store';

/**
 * 设备列表（只读）。
 *
 * **没有饮品这一屏**：饮品管理留在后台（coffee-machine-service 的 /v1/admin/coffee-machines
 * 那棵树上），这里连入口都不摆——摆一排点进去 404 的菜单比不摆更糟。
 */
const DevicesPage: React.FC = () => {
  // 门店 id → 名称。设备接口只回 storeId（它没有门店那一列的数据），要显示门店名就得自己
  // 拼一张表。用 /v1/merchant/stores 而不是后台那棵树：这张表天然被数据范围框住，范围外的
  // 门店根本不会出现在里面。
  const [storeNames, setStoreNames] = useState<Record<string, string>>({});
  const [storesTruncated, setStoresTruncated] = useState(false);

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const page = await listStores(FULL_PAGE_PARAMS);
        if (!alive) return;
        setStoreNames(Object.fromEntries(page.items.map((s) => [s.id, s.name])));
        // 超过一页时后面的门店映射不到，页面上会出现一列 uuid。这是**取不到名字**，不是
        // 「这台设备没有门店」——所以下面要显式说出来，不能让它安静地铺在表里。
        setStoresTruncated(page.total > page.items.length);
      } catch {
        // 名字取不到不该拦住设备列表：下面会把 storeId 原样显示出来，那仍然是个有用的值。
        if (alive) setStoreNames({});
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  const storeCell = (storeId: string | null) => {
    if (!storeId) return '—';
    return storeNames[storeId] ?? storeId;
  };

  const columns: ProColumns<DeviceSummary>[] = [
    {
      // 这两列**不开各自的搜索项**：接口（merchant_device.go 的 List）只认
      // keyword / status / manufacturerId 三个键，设备名与机器编码的模糊匹配都走 keyword。
      // 不关掉的话筛选栏会多出两个筛不动的框，填了只会原样返回全部行。
      title: '设备名称',
      dataIndex: 'deviceName',
      width: 180,
      ellipsis: true,
      hideInSearch: true,
      render: (_, r) => r.deviceName || '—',
    },
    {
      title: '机器编码',
      dataIndex: 'serialUnique',
      width: 200,
      ellipsis: true,
      copyable: true,
      hideInSearch: true,
    },
    {
      // keyword 是服务端的关键词（设备名 / 机器编码模糊匹配），不对应表里任何一列，
      // 所以它只在筛选栏里出现——也是**筛选栏里唯一一个**能按名字找设备的框。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '设备名或机器编码' },
    },
    {
      title: '门店',
      dataIndex: 'storeId',
      width: 180,
      ellipsis: true,
      hideInSearch: true,
      render: (_, r) => storeCell(r.storeId),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      valueType: 'select',
      valueEnum: searchOptions(DEVICE_STATUS),
      render: (_, row) => {
        const meta = enumMeta(DEVICE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '厂商在线',
      dataIndex: 'vendorOnline',
      width: 110,
      hideInSearch: true,
      render: (_, row) => {
        const meta = vendorOnlineMeta(row.vendorOnline);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '最近故障',
      dataIndex: 'lastFaultMessage',
      width: 220,
      ellipsis: true,
      hideInSearch: true,
      // 故障码与描述一起显示：光有「机器故障」这四个字看不出该找谁修。
      render: (_, row) =>
        row.lastFaultMessage ? `${row.lastFaultCode} ${row.lastFaultMessage}` : '—',
    },
    {
      title: '最近活跃',
      dataIndex: 'lastActiveAt',
      width: 170,
      valueType: 'dateTime',
      hideInSearch: true,
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      valueType: 'dateTime',
      width: 170,
      hideInSearch: true,
    },
    {
      title: '操作',
      valueType: 'option',
      // 这一列钉在右边，宽度必须给够：给少了溢出的按钮会直接落在表格外面。
      width: 90,
      fixed: 'right',
      render: (_, row) => (
        <Button type="link" size="small" onClick={() => history.push(`/devices/${row.id}`)}>
          查看
        </Button>
      ),
    },
  ];

  return (
    <PageContainer title="设备">
      {storesTruncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="门店超过 200 家，部分设备显示的是门店 ID 而不是门店名"
        />
      )}
      <ProTable<DeviceSummary>
        rowKey="id"
        columns={columns}
        // 必须等于各列 width 之和：180+200+180+100+110+220+170+170+90=1420。
        // 比实际列宽之和小的话，钉在右边的操作列跟表体是错开的。
        scroll={{ x: 1420 }}
        options={{ reload: true, density: false, setting: true }}
        // search 里的 keyword 直接透传给后端：后端按设备名 / 机器编码模糊匹配，所以它在
        // 筛选栏里是**独立一项**，没有 dataIndex 上的列与之对应。
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const result = await listDevices(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
      />
    </PageContainer>
  );
};

export default DevicesPage;
