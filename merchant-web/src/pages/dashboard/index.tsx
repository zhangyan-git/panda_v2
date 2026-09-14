import { PageContainer, StatisticCard } from '@ant-design/pro-components';

const DashboardPage: React.FC = () => (
  <PageContainer title="概览">
    <StatisticCard.Group>
      <StatisticCard statistic={{ title: '今日订单', value: 0 }} />
      <StatisticCard statistic={{ title: '咖啡机总数', value: 0 }} />
    </StatisticCard.Group>
  </PageContainer>
);

export default DashboardPage;
