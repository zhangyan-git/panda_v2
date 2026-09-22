import { PlusOutlined } from '@ant-design/icons';
import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Alert, Button, Descriptions, Drawer, message, Popconfirm, Tag, Typography } from 'antd';
import { useAccess } from '@umijs/max';
import { useCallback, useEffect, useState } from 'react';
import { formatDateTime } from '../../services/datetime';
import { enumMeta } from '../../services/labels';
import {
  getPartner,
  listPartnerKeys,
  updatePartnerKeyStatus,
  type APIKeyIssued,
  type Partner,
  type PartnerAPIKey,
} from '../../services/partner';
import { API_KEY_STATUS, PARTNER_STATUS } from '../../services/partnerLabels';
import { requestErrorMessage } from '../../services/requestError';
import KeyFormModal from './KeyFormModal';

/**
 * 一家合作方名下的密钥抽屉。
 *
 * # 为什么密钥挂在合作方下面、而不是单独一页
 *
 * 「这家公司还能不能调」与「他手上那几把钥匙是什么状态」是同一个问题的两面：合作方一停用，
 * 下面每一把都当场失效（后端验签那条路现查合作方那一行）。拆成两页只会让人来回翻着对。
 * 调用日志不在这个抽屉里——它按合作方与时间段筛，需要的是查询区而不是这一行的详情。
 *
 * # 头部那份合作方信息是**每次打开时重取**的
 *
 * 抽屉里会改 keyCount（签发、停用都不删行）与 status（别处停用了这家），而列表上那一行是
 * 打开抽屉那一刻的快照。头部显示「密钥数 2」而下面列着 3 把，是这一页最容易出现的一种
 * 「数据对不上」——它其实只是没重取。
 *
 * # 明文不经过这个组件
 *
 * 签发响应由 onIssued 原样交给页面那个一次性弹窗（IssuedKeyModal），这个抽屉从头到尾只
 * 见过掩码。原因是这里的状态活得比那一次回显长得多：它会一直挂到用户关掉抽屉（关抽屉之前
 * 还会刷新好几次列表），明文住在这里就等于「回显了很多次」。
 */
type Props = {
  open: boolean;
  /** undefined = 抽屉关着。 */
  partner?: Partner;
  onClose: () => void;
  /** 签发成功 → 交给页面那个一次性弹窗。 */
  onIssued: (issued: APIKeyIssued) => void;
  /** 抽屉里的任何改动都要让外面那张表重拉一次：keyCount 变了。 */
  onChanged: () => void;
};

export default function KeysDrawer({
  open,
  partner,
  onClose,
  onIssued,
  onChanged,
}: Props) {
  const access = useAccess();
  const [keys, setKeys] = useState<PartnerAPIKey[]>([]);
  const [loading, setLoading] = useState(false);
  /** 抽屉头部用的那份合作方：比列表那一行新，见上面那段。 */
  const [detail, setDetail] = useState<Partner>();
  // 签发（undefined）与修改共用一张表单，开合单独一个 state（只用 editing 做不到「新增」：
  // 那两件事的 editing 都是 undefined，与「关掉了」长得一模一样）。
  const [editingKey, setEditingKey] = useState<PartnerAPIKey>();
  const [keyFormOpen, setKeyFormOpen] = useState(false);

  const load = useCallback(async () => {
    if (!partner) return;
    setLoading(true);
    try {
      // 两个请求并发：一个是这一行的详情（给头部），一个是它名下的密钥。
      const [refreshed, page] = await Promise.all([
        getPartner(partner.id),
        listPartnerKeys(partner.id),
      ]);
      setDetail(refreshed);
      setKeys(page.items);
    } catch (error) {
      message.error(requestErrorMessage(error, '加载密钥失败'));
    } finally {
      setLoading(false);
    }
  }, [partner]);

  useEffect(() => {
    // 关着的时候不拉：open=false 只发生在关闭动画那几帧里（调用处按 partner 换 key，
    // 换行时整个组件重挂）。
    if (open) void load();
  }, [open, load]);

  /** 启停一把钥匙。与「修改」分开走一条路径：后端也是 PATCH 只改 status。 */
  const changeKeyStatus = async (row: PartnerAPIKey, status: 'enabled' | 'disabled') => {
    if (!partner) return;
    try {
      await updatePartnerKeyStatus(partner.id, row.id, status);
    } catch (error) {
      message.error(requestErrorMessage(error, status === 'enabled' ? '启用失败' : '停用失败'));
      return;
    }
    message.success(status === 'enabled' ? '已启用' : '已停用');
    void load();
    onChanged();
  };

  const columns: ProColumns<PartnerAPIKey>[] = [
    {
      title: '备注名',
      dataIndex: 'name',
      width: 140,
      ellipsis: true,
      // 备注名可以为空（它是给人看的，不是判据），空着显示「—」。
      render: (_, row) => (row.name ? row.name : '—'),
    },
    {
      // 掩码是「前 4 + … + 后 4」。**不是**可复制来用的东西：真正的明文只在签发那一刻出现过。
      title: 'API Key',
      dataIndex: 'apiKeyMask',
      width: 150,
      render: (_, row) => <Typography.Text code>{row.apiKeyMask}</Typography.Text>,
    },
    {
      title: '签名密钥',
      dataIndex: 'secretMask',
      width: 150,
      render: (_, row) => <Typography.Text code>{row.secretMask}</Typography.Text>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 80,
      render: (_, row) => {
        const meta = enumMeta(API_KEY_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '有效期',
      dataIndex: 'expiresAt',
      width: 160,
      render: (_, row) =>
        row.expiresAt ? formatDateTime(row.expiresAt) : <Typography.Text type="secondary">长期有效</Typography.Text>,
    },
    {
      title: '来源白名单',
      dataIndex: 'ipWhitelist',
      width: 200,
      ellipsis: true,
      // 空数组 = **不限制来源**（不是全拒）。这一格写「不限制」而不是「—」：后者看起来像
      // 没配，而这两件事的下一步动作正好相反。
      render: (_, row) =>
        row.ipWhitelist.length > 0 ? (
          <Typography.Text>{row.ipWhitelist.join('、')}</Typography.Text>
        ) : (
          <Typography.Text type="secondary">不限制来源</Typography.Text>
        ),
    },
    {
      title: '额度',
      dataIndex: 'rateLimitPerMinute',
      width: 100,
      render: (_, row) => `${row.rateLimitPerMinute}/分钟`,
    },
    {
      title: '最近使用',
      dataIndex: 'lastUsedAt',
      width: 160,
      // 从未用过是 null（不是它坏了）：一把从没被调用过的钥匙，与一把调用停了很久的钥匙，
      // 是两种不同的处境——前者多半是还没联调，后者才是「他是不是不用了」。
      render: (_, row) =>
        row.lastUsedAt ? formatDateTime(row.lastUsedAt) : <Typography.Text type="secondary">从未使用</Typography.Text>,
    },
    { title: '调用次数', dataIndex: 'callCount', width: 90 },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且上面的 scroll.x 必须等于各列 width 之和。
      width: 130,
      fixed: 'right',
      render: (_, row) =>
        access.canManagePartners
          ? [
              <Button
                key="edit"
                type="link"
                size="small"
                onClick={() => {
                  setEditingKey(row);
                  setKeyFormOpen(true);
                }}
              >
                修改
              </Button>,
              row.status === 'enabled' ? (
                <Popconfirm
                  key="disable"
                  title="停用这一把密钥？"
                  // 纯文本节点，别在这里写 markdown 星号——它不会被渲染成加粗。
                  description="停用后它从下一个请求起就被拒（后端每请求现查，立即生效）。这是怀疑泄露时最快的动作：不用等对方改代码。停用不是删除，随时可以再启用。"
                  okText="停用"
                  onConfirm={() => changeKeyStatus(row, 'disabled')}
                >
                  <Button type="link" size="small" danger>
                    停用
                  </Button>
                </Popconfirm>
              ) : (
                <Popconfirm
                  key="enable"
                  title="启用这一把密钥？"
                  description="启用后它立刻重新生效。启用之前先确认对方的签名已经改过来了。"
                  okText="启用"
                  onConfirm={() => changeKeyStatus(row, 'enabled')}
                >
                  <Button type="link" size="small">
                    启用
                  </Button>
                </Popconfirm>
              ),
            ]
          : [],
    },
  ];

  return (
    <Drawer
      title={partner ? `${partner.name}（${partner.code}）的密钥` : '密钥'}
      open={open}
      onClose={onClose}
      width={1120}
      destroyOnClose
    >
      {detail && (
        <Descriptions size="small" column={2} style={{ marginBottom: 16 }}>
          <Descriptions.Item label="编码">
            <Typography.Text copyable>{detail.code}</Typography.Text>
          </Descriptions.Item>
          <Descriptions.Item label="状态">
            {/* 合作方的启停不在这里改（它连坐），只显示当下是什么状态。 */}
            <Tag color={enumMeta(PARTNER_STATUS, detail.status).color}>
              {enumMeta(PARTNER_STATUS, detail.status).text}
            </Tag>
          </Descriptions.Item>
          <Descriptions.Item label="有效期">
            {detail.expiresAt ? formatDateTime(detail.expiresAt) : '长期有效'}
          </Descriptions.Item>
          <Descriptions.Item label="密钥数">
            {/* 含已停用的：停一把之后总数不变，才知道那把还在。
                **取列表自己的长度，不取 detail.keyCount**——合作方详情那条接口的映射器
                （partner-service controller 的 partnerItemFromModel）不填 KeyCount，只有
                列表那条查询才 JOIN 它，所以详情里这个字段恒为 0：头部写「密钥数 0」而下面
                列着 N 把。下面这张表就是**该合作方名下全部密钥**（listPartnerKeys 不分页，
                见 services/partner.ts），它的长度与头部要说的那个数是同一个事实，而且与
                表格里看得见的行数永远不会对不上。 */}
            {keys.length}
          </Descriptions.Item>
          <Descriptions.Item label="联系人">{detail.contactName || '—'}</Descriptions.Item>
          <Descriptions.Item label="联系电话">{detail.contactPhone || '—'}</Descriptions.Item>
        </Descriptions>
      )}
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="这里改的是「谁能调进来」，不是合作方本身"
        // 这里不写 markdown 星号：Alert 的 description 是纯文本节点，星号不会被渲染成加粗。
        description="停用一把密钥下一个请求就生效。停用合作方不在这张抽屉里——那会让下面每一把（包括状态还是启用的）当场全部失效，所以它在列表那一行上单独确认。"
      />
      <ProTable<PartnerAPIKey>
        rowKey="id"
        columns={columns}
        dataSource={keys}
        loading={loading}
        search={false}
        pagination={false}
        options={false}
        size="small"
        // 1360 = 140+150+150+80+160+200+100+160+90+130，各列 width 之和（含钉右那列）。
        // 抽屉宽 1120 装不下，所以这里必须有 scroll.x，否则钉右的那一列会被内容顶出去。
        scroll={{ x: 1360 }}
        toolBarRender={() =>
          access.canManagePartners
            ? [
                <Button
                  key="issue"
                  type="primary"
                  icon={<PlusOutlined />}
                  onClick={() => {
                    setEditingKey(undefined);
                    setKeyFormOpen(true);
                  }}
                >
                  签发新密钥
                </Button>,
              ]
            : []
        }
        locale={{
          emptyText: (
            <div style={{ padding: '32px 0', color: '#999' }}>
              这家合作方还没有密钥，接口一把也调不通。点右上角「签发新密钥」——明文只会显示一次。
            </div>
          ),
        }}
      />

      {/*
        签发 / 修改共用一张表单。key 用 id 而不是固定值：编辑不同行时强制换一个 Form 实例，
        与弹窗内部的 destroyOnClose 一起，挡住「把上一把的白名单写到下一把上」那个坑。
        关窗时同时清 editing——只清 open 的话，下次点「签发」会带着上一把的 id 进去，
        把签发当成修改提交。
      */}
      {partner && (
        <KeyFormModal
          key={editingKey?.id ?? 'new'}
          open={keyFormOpen}
          partnerId={partner.id}
          editing={editingKey}
          onOpenChange={(next) => {
            setKeyFormOpen(next);
            if (!next) setEditingKey(undefined);
          }}
          onIssued={(issued) => {
            // 明文原样交给页面那个一次性弹窗，**不进这个组件的任何 state**（见文件头那段）。
            onIssued(issued);
            // 列表照常刷新，但拿到的只有掩码——「刷新之后明文还在」这件事在数据上就不可能发生。
            void load();
            onChanged();
          }}
          onSaved={() => {
            void load();
            onChanged();
          }}
        />
      )}
    </Drawer>
  );
}
