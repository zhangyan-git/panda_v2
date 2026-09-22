import { PageContainer } from '@ant-design/pro-components';
import { Card, Empty } from 'antd';

/**
 * 概览。
 *
 * 这里原先摆着三张 StatisticCard（咖啡机总数 / 今日订单 / 在线商户），值一律硬编码 0——
 * 没有 request、也没有「暂无」的标注。本仓**没有统计接口**，那三个 0 会被读成
 * 「一台咖啡机都没有 / 今天一单都没有 / 一家商户都没上线」，是比空白更坏的一种错。
 *
 * 所以整组卡片先撤掉，改成一句说清楚的空状态。这不是「先做成 0」的临时脚手架：全仓同一条
 * 口径——没有接口的东西不伪装成数据。等统计接口
 * （或某个现有接口能把这三个数聚合出来）落地，再把 StatisticCard.Group 接回来。
 */
const DashboardPage: React.FC = () => (
  <PageContainer title="概览">
    <Card>
      {/* 用 Empty description 表达「没有东西可看」是全仓的写法（列表页给自定义 emptyText、
          详情页给 <Empty description={error} /> 都是它）。 */}
      <Empty
        image={Empty.PRESENTED_IMAGE_SIMPLE}
        description="概览统计还没有后端来源：本仓没有统计接口，所以这里不显示咖啡机总数 / 今日订单 / 在线商户。要看这三样，去设备管理、订单列表与商户管理各自的列表页。"
      />
    </Card>
  </PageContainer>
);

export default DashboardPage;
