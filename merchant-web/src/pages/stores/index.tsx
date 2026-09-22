import { PageContainer, ProTable } from '@ant-design/pro-components';
import { history } from '@umijs/max';
import { Button, Space, Tag } from 'antd';
import type { ProColumns } from '@ant-design/pro-components';
import { enumMeta, searchOptions } from '../../services/labels';
import { toPageParams } from '../../services/pagination';
import { listStores, type Store } from '../../services/store';
import { STORE_AUDIT_STATUS, STORE_STATUS } from '../../services/storeLabels';

/**
 * 门店列表（只读）。
 *
 * 这里**没有**筛选栏里的「所属商户」：一个商户账号的数据范围里只有自己家的门店（服务端按
 * 令牌解析出来的授权点位集合过滤），摆一个只有一项的下拉没有意义。品牌也不摆——那是后台
 * 的事，商户端只要看得见「这家店归哪个品牌」就够了。
 *
 * **筛选栏里只留后端真认的两个键。** ProTable 默认给每个列都生成一个搜索项，而这个接口
 * （MerchantStoreHandler.List）只读 name / status：品牌、地址、电话、创建时间这四列要是
 * 不显式关掉搜索，用户填进去点「查询」会原样返回全部行——一个筛不动却看着能筛的输入框，
 * 比没有这个框更糟。已用 curl 实测：`?brandName=…` 回全量，`?name=…` 才收窄。
 */
const StoresPage: React.FC = () => {
  const columns: ProColumns<Store>[] = [
    { title: '门店名称', dataIndex: 'name', width: 200, ellipsis: true },
    { title: '所属品牌', dataIndex: 'brandName', width: 160, ellipsis: true, search: false },
    {
      title: '地址',
      dataIndex: 'address',
      width: 240,
      ellipsis: true,
      search: false,
      // 空地址显示「—」而不是留白，免得看起来像加载失败。
      render: (_, r) =>
        [r.province, r.city, r.district, r.address].filter(Boolean).join('') || '—',
    },
    {
      title: '联系电话',
      dataIndex: 'phone',
      width: 140,
      search: false,
      render: (_, r) => r.phone || '—',
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      valueType: 'select',
      valueEnum: searchOptions(STORE_STATUS),
      render: (_, row) => {
        const meta = enumMeta(STORE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '审核状态',
      dataIndex: 'auditStatus',
      width: 110,
      valueType: 'select',
      valueEnum: searchOptions(STORE_AUDIT_STATUS),
      search: false,
      // 审核备注不进筛选、也不单独占一列，用 tooltip 挂在标签上：一段自由文本撑不满一列，
      // 但驳回的理由又是最该被看见的那句话。
      render: (_, row) => {
        const meta = enumMeta(STORE_AUDIT_STATUS, row.auditStatus);
        const node = <Tag color={meta.color}>{meta.text}</Tag>;
        return row.auditRemark ? (
          <Space direction="vertical" size={0}>
            {node}
            <span style={{ color: 'rgba(0,0,0,0.45)', fontSize: 12 }}>{row.auditRemark}</span>
          </Space>
        ) : (
          node
        );
      },
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
        <Button type="link" size="small" onClick={() => history.push(`/stores/${row.id}`)}>
          查看
        </Button>
      ),
    },
  ];

  return (
    <PageContainer title="门店">
      <ProTable<Store>
        rowKey="id"
        columns={columns}
        // 必须等于各列 width 之和：200+160+240+140+100+110+170+90=1210。
        // 比实际列宽之和小的话，钉在右边的操作列跟表体是错开的。
        scroll={{ x: 1210 }}
        options={{ reload: true, density: false, setting: true }}
        request={async (params) => {
          const result = await listStores(toPageParams(params));
          // total 必须是服务端给的总数而不是本页条数，否则分页器只剩一页。
          return { data: result.items, total: result.total, success: true };
        }}
      />
    </PageContainer>
  );
};

export default StoresPage;
