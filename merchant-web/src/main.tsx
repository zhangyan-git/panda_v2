import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Alert, Card, Layout, Typography } from 'antd';
import React from 'react';
import { createRoot } from 'react-dom/client';
import 'antd/dist/reset.css';

const client = new QueryClient();
function App() { return <Layout style={{ minHeight: '100vh', padding: 32 }}><Card><Typography.Title level={2}>Panda V2</Typography.Title><Alert message="Platform skeleton" description="业务页面和领域流程将在范围确认后实现。" type="info" /></Card></Layout>; }
createRoot(document.getElementById('root')!).render(<React.StrictMode><QueryClientProvider client={client}><App /></QueryClientProvider></React.StrictMode>);
