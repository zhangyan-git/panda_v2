import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Descriptions, Empty, Image, Tag } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { enumMeta } from '../../../services/labels';
import { requestErrorMessage } from '../../../services/session';
import { getStore, type Store } from '../../../services/store';
import { STORE_AUDIT_STATUS, STORE_STATUS } from '../../../services/storeLabels';

/** 空值统一显示「—」：留白看起来像加载失败，空串看起来像页面出错。 */
const dash = (value?: string | null) => (value?.trim() ? value : '—');

/**
 * 门店详情（只读）。
 *
 * 取不到有两种原因，服务端把它们并成了同一个 404（范围外与不存在），所以这里也只能给出
 * 一个结论：「这家门店不存在或不在你的数据范围内」。分开写会反过来泄露「这个 id 是存在
 * 的，只是不属于你」——拿别人的 id 逐个试一遍就能数出对方有哪些门店。
 */
const StoreDetailPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const [store, setStore] = useState<Store>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setStore(await getStore(id));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载门店失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  const statusMeta = store ? enumMeta(STORE_STATUS, store.status) : undefined;
  const auditMeta = store ? enumMeta(STORE_AUDIT_STATUS, store.auditStatus) : undefined;

  return (
    <PageContainer
      loading={loading}
      title={store?.name || '门店详情'}
      subTitle={store?.brandName}
      onBack={() => history.push('/stores')}
    >
      {error ? (
        <Card>
          <Empty description={error} />
        </Card>
      ) : (
        <Card>
          <Descriptions column={2} bordered size="middle">
            <Descriptions.Item label="门店名称">{dash(store?.name)}</Descriptions.Item>
            <Descriptions.Item label="所属品牌">{dash(store?.brandName)}</Descriptions.Item>
            <Descriptions.Item label="所属商户">{dash(store?.merchantName)}</Descriptions.Item>
            <Descriptions.Item label="状态">
              {statusMeta ? <Tag color={statusMeta.color}>{statusMeta.text}</Tag> : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="审核状态">
              {auditMeta ? <Tag color={auditMeta.color}>{auditMeta.text}</Tag> : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="审核备注">{dash(store?.auditRemark)}</Descriptions.Item>
            <Descriptions.Item label="地址" span={2}>
              {dash(
                [store?.province, store?.city, store?.district, store?.address]
                  .filter(Boolean)
                  .join(''),
              )}
            </Descriptions.Item>
            <Descriptions.Item label="联系电话">{dash(store?.phone)}</Descriptions.Item>
            <Descriptions.Item label="营业时间">{dash(store?.businessHours)}</Descriptions.Item>
            <Descriptions.Item label="联系人">{dash(store?.contactName)}</Descriptions.Item>
            <Descriptions.Item label="联系人电话">{dash(store?.contactPhone)}</Descriptions.Item>
            <Descriptions.Item label="客户编码">{dash(store?.customerCode)}</Descriptions.Item>
            <Descriptions.Item label="DMS 编码">{dash(store?.dmsCode)}</Descriptions.Item>
            <Descriptions.Item label="客户类型">{dash(store?.customerType)}</Descriptions.Item>
            <Descriptions.Item label="在商户端可见">
              {store ? (store.visible ? '是' : '否') : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="门店详情" span={2}>
              {dash(store?.detail)}
            </Descriptions.Item>
            <Descriptions.Item label="备注" span={2}>
              {dash(store?.remark)}
            </Descriptions.Item>
            <Descriptions.Item label="创建时间" span={2}>
              {dash(store?.createdAt)}
            </Descriptions.Item>
            {/* 照片为空时不摆这一行：一个空白的 Image 框看起来像图裂了。 */}
            {store?.photos?.length ? (
              <Descriptions.Item label="门店照片" span={2}>
                <Image.PreviewGroup>
                  {store.photos.map((url) => (
                    <Image key={url} src={url} width={96} />
                  ))}
                </Image.PreviewGroup>
              </Descriptions.Item>
            ) : null}
          </Descriptions>
        </Card>
      )}
    </PageContainer>
  );
};

export default StoreDetailPage;
