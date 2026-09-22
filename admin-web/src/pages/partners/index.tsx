import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Alert, Button, message, Popconfirm, Tag, Typography } from 'antd';
import { useCallback, useEffect, useRef, useState } from 'react';
import { formatDateTime } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { FULL_PAGE_PARAMS } from '../../services/pagination';
import {
  listPartners,
  updatePartnerStatus,
  type APIKeyIssued,
  type Partner,
  type PartnerStatus,
} from '../../services/partner';
import { PARTNER_STATUS } from '../../services/partnerLabels';
import { requestErrorMessage } from '../../services/requestError';
import CallLogsTable from './CallLogsTable';
import IssuedKeyModal from './IssuedKeyModal';
import KeysDrawer from './KeysDrawer';
import PartnerFormModal from './PartnerFormModal';

/**
 * 开放平台 / 合作方（方案 §3.4）。
 *
 * # 一页里有三件事
 *
 * 合作方（列表、新增、编辑、启停）、密钥（在行上的抽屉里：签发、改白名单与额度、启停）、
 * 调用日志（页尾的查询区）。菜单只有「合作方」一片叶子（migrations/identity/028）：合作方与
 * 密钥必须在一起看——合作方一停用，名下**所有**密钥当场失效，拆成两页只会让人来回翻着对；
 * 调用日志作为同一页上的查询区，是因为它按合作方与时间段筛，是一个查询动作而不是一行详情。
 *
 * # 这一页管的是「谁能打进来」
 *
 * 与支付方式与渠道那一页同一个性质，但更要紧一档：这里的每一次改动都直接改「哪些请求会被
 * 放行」。所以三件事是分开的三个动作——启停合作方有二次确认（连坐）、启停密钥有二次确认
 * （下一个请求生效）、改配置走整份覆盖的 PUT。**没有删除**：密钥的「不再使用」是停用而不是
 * 删行（调用日志还指着它的 api_key_id），合作方同理。
 *
 * # 明文密钥只回显一次
 *
 * 签发那一次之后再也拿不到（库里只有掩码，后端读路径一次解密都不做）。这条规则在前端只有
 * 一个落点：`issued` 这一个 state 槽 + 条件挂载的 IssuedKeyModal，关掉即清空。**不要**把明文
 * 并进 partners / keys 那几份列表 state——那会让它在之后任何一次刷新或重渲染里继续存在，
 * 而页面上看不出区别（见 IssuedKeyModal.tsx 顶部那段）。
 *
 * # 权限
 *
 * 看这一页是 `partner:read`，改（新增 / 编辑 / 启停 / 签发 / 改白名单）是 `partner:manage`。
 * 没有 manage 的人看到的仍是一张能查的页：所有写按钮与操作列都不出现。**manage 不等于能看到
 * 明文**——它给的最后一件事是「再签发一把」，不是「把已有的那把读出来」。
 */
export default function PartnersPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  /**
   * 全部合作方（一次拉满页），**只给调用日志那个筛选下拉用**。
   *
   * 它不能复用下面表格的数据源：那张表是服务端分页的，手上只有当前这一页，用它做下拉会让
   * 「筛一家不在第一页的合作方」直接选不到——而那种时候人只会以为「这个人没调用过」。
   */
  const [allPartners, setAllPartners] = useState<Partner[]>([]);
  const [editingPartner, setEditingPartner] = useState<Partner>();
  const [partnerFormOpen, setPartnerFormOpen] = useState(false);
  /** 密钥抽屉盯住的那一行（undefined = 抽屉关着）。 */
  const [keyPartner, setKeyPartner] = useState<Partner>();
  /** 签发出来、还没被用户关掉的那一份明文。**它只活在这里**，见文件头那段。 */
  const [issued, setIssued] = useState<APIKeyIssued>();

  const loadAllPartners = useCallback(async () => {
    try {
      const page = await listPartners(FULL_PAGE_PARAMS);
      setAllPartners(page.items);
    } catch (error) {
      // 不整页报错：这一条只喂那个下拉框，列表本身照常能看能筛（它自己那条请求在表格里）。
      message.error(requestErrorMessage(error, '加载合作方下拉失败'));
    }
  }, []);

  useEffect(() => {
    void loadAllPartners();
  }, [loadAllPartners]);

  /** 表格与下拉一起刷新：写操作两处都可能受影响（新增合作方、改名、启停）。 */
  const reloadAll = () => {
    actionRef.current?.reload();
    void loadAllPartners();
  };

  /**
   * 启停一家合作方。
   *
   * 与「编辑表单」分开走一条路径（后端也是 PATCH 只改 status）：停用会让它名下**所有**密钥
   * 当场失效，那件事不该与「顺手改个联系电话」共用一次提交。
   */
  const changePartnerStatus = async (row: Partner, status: PartnerStatus) => {
    try {
      await updatePartnerStatus(row.id, status);
    } catch (error) {
      message.error(requestErrorMessage(error, status === 'enabled' ? '启用失败' : '停用失败'));
      return;
    }
    message.success(status === 'enabled' ? '已启用' : '已停用');
    reloadAll();
  };

  const columns: ProColumns<Partner>[] = [
    {
      title: '编码',
      dataIndex: 'code',
      copyable: true,
      width: 150,
      // 一个框搜两个字段（后端 ILIKE，同时匹配编码与名称）：运营手上要么是编码（对接方报过来
      // 的），要么是名字（他自己起的），让他先选搜哪个字段只是多一步。
      fieldProps: { placeholder: '编码或名称，支持模糊匹配' },
    },
    { title: '名称', dataIndex: 'name', hideInSearch: true, width: 180, ellipsis: true },
    {
      title: '联系人',
      dataIndex: 'contactName',
      hideInSearch: true,
      width: 100,
      render: (_, row) => row.contactName || '—',
    },
    {
      title: '联系电话',
      dataIndex: 'contactPhone',
      hideInSearch: true,
      width: 140,
      render: (_, row) => row.contactPhone || '—',
    },
    {
      title: '联系邮箱',
      dataIndex: 'contactEmail',
      hideInSearch: true,
      width: 200,
      ellipsis: true,
      render: (_, row) => row.contactEmail || '—',
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(PARTNER_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(PARTNER_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '有效期',
      dataIndex: 'expiresAt',
      hideInSearch: true,
      width: 160,
      // null = 不过期，显示成「长期有效」。这是两种不同的处境：一个是「签到了某天」，一个是
      // 「一直有效」——空着看起来像漏填了。
      render: (_, row) =>
        row.expiresAt ? (
          formatDateTime(row.expiresAt)
        ) : (
          <Typography.Text type="secondary">长期有效</Typography.Text>
        ),
    },
    {
      // 含已停用的密钥（后端就是这么算的）。「一把钥匙都没有」与「他有三把」在排查时是完全
      // 不同的两个处境，而它们只能靠这个数分开。
      title: '密钥数',
      dataIndex: 'keyCount',
      hideInSearch: true,
      width: 90,
    },
    {
      title: '最近更新',
      dataIndex: 'updatedAt',
      valueType: 'dateTime',
      hideInSearch: true,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列 width 之和（见下面的 1480）。
      // 200 是按支付方式那一页量出来的：两个 2 字链接按钮占 140（含分隔线与内边距），
      // 这一列最多同时出现三个（密钥 / 编辑 / 停用），所以按每枚 2 字按钮约 65 算。
      width: 200,
      fixed: 'right',
      // 没有 manage 的人拿到的是一个**只含「密钥」的数组**，不是空数组：看掩码、白名单与
      // 限额本身就是排查的一半（「他被哪一条挡了」），而 `partner:read` 这一枚存在的理由
      // 就是让人能查而不必拿到动它的权力。抽屉里的写按钮另判同一个码。
      //
      // 返回数组而不是 null（照 payments 的做法）：多一个写操作的位置空着，
      // 比出现一个点了没反应的按钮好。
      render: (_, row) => [
        <Button key="keys" type="link" size="small" onClick={() => setKeyPartner(row)}>
          密钥
        </Button>,
        ...(access.canManagePartners
          ? [
              <Button
                key="edit"
                type="link"
                size="small"
                onClick={() => {
                  setEditingPartner(row);
                  setPartnerFormOpen(true);
                }}
              >
                编辑
              </Button>,
              row.status === 'enabled' ? (
                <Popconfirm
                  key="disable"
                  title="停用这家合作方？"
                  // 纯文本节点，别在这里写 markdown 星号——它不会被渲染成加粗。
                  description="停用后它名下所有密钥（包括状态还是启用的）从下一个请求起全部被拒：对接方的整条链路会立刻断。恢复时要把合作方重新启用，并逐把检查密钥的状态。"
                  okText="停用"
                  onConfirm={() => changePartnerStatus(row, 'disabled')}
                >
                  <Button type="link" size="small" danger>
                    停用
                  </Button>
                </Popconfirm>
              ) : (
                <Popconfirm
                  key="enable"
                  title="启用这家合作方？"
                  description="启用后它的密钥立刻重新生效——包括这段时间里没有被停用、但跟着一起失效的那些。"
                  okText="启用"
                  onConfirm={() => changePartnerStatus(row, 'enabled')}
                >
                  <Button type="link" size="small">
                    启用
                  </Button>
                </Popconfirm>
              ),
            ]
          : []),
      ],
    },
  ];

  return (
    <PageContainer
      title="合作方"
      content="接入这条开放平台的每一家公司，以及他们手上能调进来的密钥。这三件事是一条链路：合作方停用 → 名下所有密钥当场失效。"
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="这里改的是「谁能打进来」"
        description="密钥的明文只在签发那一刻显示一次，之后列表与详情里都只有掩码（库里也只有掩码）。所以「这把钥匙是不是泄漏了」的处理动作只有一个：停用它，再给对接方签发一把新的——没有「把原来的那把查出来」这条路。"
      />
      <ProTable<Partner>
        headerTitle="合作方"
        rowKey="id"
        actionRef={actionRef}
        columns={columns}
        // 1480 = 150+180+100+140+200+90+160+90+170+200，各列 width 之和（含钉右那列）。
        // 钉右列必须有它，而且每一列的宽度都要装得下自己的内容（见上面「操作」那列的注释）。
        scroll={{ x: 1480 }}
        // 服务端分页 + 服务端筛选：合作方会随接入方增长，一次拉全量迟早会撞上每页 200 的上限，
        // 而撞上的表现是「列表里少了一些合作方」——不报错。
        search={{ labelWidth: 'auto' }}
        pagination={{ defaultPageSize: 20, showSizeChanger: true }}
        request={async (params) => {
          // 逐字段挑，不整个透传：表格给的 current / pageSize 要换成接口的 page / pageSize，
          // 而 keyword / status 空着的时候要整个丢掉（多一个空串换来的是「查不到」）。
          const keyword = typeof params.code === 'string' ? params.code.trim() : '';
          try {
            const result = await listPartners({
              page: params.current,
              pageSize: params.pageSize,
              keyword: keyword || undefined,
              status: params.status || undefined,
            });
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            // 后端对不合法的筛选回 400 且带一句能看懂的话。默不作声地显示空表会让人以为
            // 「一家合作方都没接」。
            message.error(requestErrorMessage(error, '加载合作方失败'));
            return { data: [], total: 0, success: false };
          }
        }}
        toolBarRender={() =>
          access.canManagePartners
            ? [
                <Button
                  key="create"
                  type="primary"
                  onClick={() => {
                    setEditingPartner(undefined);
                    setPartnerFormOpen(true);
                  }}
                >
                  新增合作方
                </Button>,
              ]
            : []
        }
        locale={{
          emptyText: (
            <div style={{ padding: '32px 0', color: '#999' }}>
              还没有合作方。点右上角「新增合作方」建一家，再进去给它签发密钥——接口才调得通。
            </div>
          ),
        }}
      />

      {/*
        调用日志：同一页上的查询区，不是独立一页（identity/028 的注释里写着这个决定）。
        它自带搜索栏与分页，合作方列表则把筛选放在自己那张表上，两者互不干扰。
      */}
      <div style={{ marginTop: 16 }}>
        <CallLogsTable partners={allPartners} />
      </div>

      {/*
        新增 / 编辑共用一张表单。key 用 id 而不是固定值：编辑不同行时强制换一个 Form 实例，
        与弹窗内部的 destroyOnClose 一起，挡住「编完 A 再点 B 带着 A 的值」那个坑。
        关窗时同时清 editing——只清 open 的话，下次点「新增」会带着上一行进去，把编辑当新增提交。
      */}
      <PartnerFormModal
        key={editingPartner?.id ?? 'new'}
        open={partnerFormOpen}
        editing={editingPartner}
        onOpenChange={(next) => {
          setPartnerFormOpen(next);
          if (!next) setEditingPartner(undefined);
        }}
        onSaved={reloadAll}
      />

      {/*
        密钥抽屉。key 用合作方 id：换一家就整个重挂，抽屉里那份「正在编辑哪一把」的 state
        不会跟着过去（否则会把 A 的白名单写到 B 的钥匙上）。
      */}
      <KeysDrawer
        key={keyPartner?.id ?? 'none'}
        open={!!keyPartner}
        partner={keyPartner}
        onClose={() => setKeyPartner(undefined)}
        onIssued={setIssued}
        onChanged={() => actionRef.current?.reload()}
      />

      {/*
        一次性明文。**条件挂载**是这个设计的一半（另一半是关掉时 setIssued(undefined)）：
        它保证明文只在「刚签发」这一刻存在于 React 树里，关掉即随组件一起消失——不放进任何
        列表 state，也就不会被之后任何一次刷新或重渲染重新贴出来。见 IssuedKeyModal.tsx。

        挂在页面这一层、而不是抽屉里面：抽屉可以被关掉，而明文必须留到用户自己点「我已保存」
        为止（抽屉关掉的同时把密钥也弄丢，是这一页最坏的一种失手）。
      */}
      {issued && <IssuedKeyModal issued={issued} onClose={() => setIssued(undefined)} />}
    </PageContainer>
  );
}
