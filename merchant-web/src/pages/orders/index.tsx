import { PageContainer, ProTable } from '@ant-design/pro-components';
import { history } from '@umijs/max';
import { Button, Space, Tag } from 'antd';
import type { ProColumns } from '@ant-design/pro-components';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { formatYuan } from '../../services/money';
import { listOrders, type OrderSummary } from '../../services/order';
import {
  FULFILLMENT_STATUS,
  ORDER_LINE_TYPE,
  ORDER_SOURCE,
  ORDER_STATUS,
  orderComposition,
} from '../../services/orderLabels';

/** 搜索值一律 trim 后再判空：后端 orderNo 是精确匹配（`=`），多一个空格就查不到。 */
const exact = (value: unknown): string | undefined => {
  const text = typeof value === 'string' ? value.trim() : '';
  return text || undefined;
};

/**
 * 订单列表（只读）。
 *
 * 这里**没有**「按门店筛」：这一屏本身就被数据范围框住了，再给一个门店下拉，会让人以为
 * 「不选门店就是全平台」。同理没有用户那一格——后端 MerchantOrderQuery 只认
 * status / orderNo / source / createdFrom / createdTo（见 order-service 的 controller）。
 *
 * 这一屏也**没有写入口**：订单的取消/完成是用户资产上的决定，不在商户端的范围里。
 */
const OrdersPage: React.FC = () => {
  const columns: ProColumns<OrderSummary>[] = [
    {
      // 仅搜索用的时间范围。用独立的 dataIndex，不和下面那列 createdAt 共用：同名的两列
      // 会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的时间列就空了。
      title: '创建时间',
      dataIndex: 'createdRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        // 后端只认带时区的 RFC3339（纯日期串没有时区，「今天」是业务时区的今天）。
        // transform 出来的两个键会并进 request 的 params。
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { createdFrom: toRFC3339(range[0]), createdTo: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '订单号',
      dataIndex: 'orderNo',
      copyable: true,
      ellipsis: true,
      width: 190,
      fieldProps: { placeholder: '完整订单号' },
    },
    {
      // 取杯号紧挨着订单号：接电话时手上要的就是这两个——「我的号是多少」是最高频的问题。
      // 取杯口屏幕上叫它取杯码，是**同一个值**，不是凭据（见 migrations/order）。
      title: '取杯号',
      dataIndex: 'pickupCode',
      width: 100,
      search: false,
      render: (_, row) => row.pickupCode || '—',
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      valueType: 'select',
      valueEnum: searchOptions(ORDER_STATUS),
      render: (_, row) => {
        const meta = enumMeta(ORDER_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '履约',
      dataIndex: 'fulfillmentStatus',
      width: 110,
      search: false,
      // 与订单状态分开显示：订单可能是「已支付」而履约「失败」——收了钱没出杯，这个组合
      // 是最需要被看见的，合成一个状态它就消失了。
      render: (_, row) => {
        const meta = enumMeta(FULFILLMENT_STATUS, row.fulfillmentStatus);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '来源',
      dataIndex: 'source',
      width: 100,
      valueType: 'select',
      valueEnum: searchOptions(ORDER_SOURCE),
      render: (_, row) => {
        const meta = enumMeta(ORDER_SOURCE, row.source);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // V2 的订单是**合并单**：一张单可以同时含饮品、加购、会员。所以「这单是哪一类」没有
      // 唯一答案，构成列把含的那几类都摆出来。
      title: '构成',
      dataIndex: 'hasDrinkLine',
      width: 170,
      search: false,
      render: (_, row) => {
        const parts = orderComposition(row);
        if (!parts.length) return '—';
        return (
          <Space size={4} wrap>
            {parts.map((lineType) => {
              const meta = enumMeta(ORDER_LINE_TYPE, lineType);
              return (
                <Tag key={lineType} color={meta.color}>
                  {meta.text}
                </Tag>
              );
            })}
          </Space>
        );
      },
    },
    {
      title: '门店',
      dataIndex: 'storeName',
      width: 160,
      ellipsis: true,
      // 不给搜索项：order-service 的 MerchantOrderQuery 只认
      // status / orderNo / source / createdFrom / createdTo，门店名传过去会被丢掉，
      // 筛了等于没筛。
      search: false,
      // 接口带的就是按 id 查出来的名字；门店被删过的历史单会缺名字，退回显示 id。
      render: (_, row) => row.storeName || row.storeId || '—',
    },
    {
      // 用户 ID 只展示、不筛选：后端 MerchantOrderQuery 里没有 userId，摆一个筛不动的输入框
      // 比不摆更糟。客服要定位一单靠的是订单号或取杯号。
      title: '用户 ID',
      dataIndex: 'userId',
      width: 150,
      ellipsis: true,
      copyable: true,
      search: false,
    },
    {
      title: '实付',
      dataIndex: 'paidAmount',
      width: 100,
      search: false,
      render: (_, row) => `¥${formatYuan(row.paidAmount)}`,
    },
    {
      // 下单时承诺赠送的福卡张数。>0 的单在后台审核退款时要有人确认没抽过奖——商户端只读，
      // 显示它是为了让门店在用户问起时说得清这一单含什么。
      title: '承诺福卡',
      dataIndex: 'fortuneCardsExpected',
      width: 100,
      search: false,
      render: (_, row) => (row.fortuneCardsExpected > 0 ? row.fortuneCardsExpected : '—'),
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
        <Button type="link" size="small" onClick={() => history.push(`/orders/${row.id}`)}>
          查看
        </Button>
      ),
    },
  ];

  return (
    <PageContainer title="订单">
      <ProTable<OrderSummary>
        rowKey="id"
        columns={columns}
        // 必须等于各列 width 之和：190+100+100+110+100+170+160+150+100+100+170+90=1540。
        // 比实际列宽之和小的话，钉在右边的操作列跟表体是错开的。
        scroll={{ x: 1540 }}
        options={{ reload: true, density: false, setting: true }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          // 逐字段挑，不整个透传：createdRange 不是接口参数，透传过去只是给后端多一个它
          // 不认识的查询键。
          const result = await listOrders({
            page: params.current,
            pageSize: params.pageSize,
            orderNo: exact(params.orderNo),
            status: exact(params.status) as OrderSummary['status'] | undefined,
            source: exact(params.source) as OrderSummary['source'] | undefined,
            createdFrom: toRFC3339(params.createdFrom),
            createdTo: toRFC3339(params.createdTo),
          });
          return { data: result.items, total: result.total, success: true };
        }}
      />
    </PageContainer>
  );
};

export default OrdersPage;
