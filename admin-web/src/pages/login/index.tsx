import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { LoginForm, ProFormText } from '@ant-design/pro-components';
import { history, useModel } from '@umijs/max';
import { message } from 'antd';
import { fetchCurrentUser, login } from '../../services/user';

const LoginPage: React.FC = () => {
  const { setInitialState } = useModel('@@initialState');

  const handleSubmit = async (values: { username: string; password: string }) => {
    try {
      const tokens = await login(values);
      localStorage.setItem('panda.auth.tokens', JSON.stringify(tokens));
      // token 已存入，拦截器现在能注入 Authorization，直接拉取用户信息
      const currentUser = await fetchCurrentUser();
      await setInitialState((s: any) => ({
        ...s,
        currentUser,
        permissions: currentUser.permissions ?? [],
      }));
      message.success('登录成功');
      history.push('/dashboard');
    } catch (err) {
      localStorage.removeItem('panda.auth.tokens');
      const msg = err instanceof Error ? err.message : '登录失败，请检查用户名和密码';
      message.error(msg);
    }
  };

  return (
    <div
      style={{
        height: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: 'oklch(0.97 0 0)',
      }}
    >
      <LoginForm
        title="Panda 后台"
        subTitle="咖啡机管理平台"
        onFinish={handleSubmit}
      >
        <ProFormText
          name="username"
          fieldProps={{ prefix: <UserOutlined /> }}
          placeholder="用户名"
          rules={[{ required: true, message: '请输入用户名' }]}
        />
        <ProFormText.Password
          name="password"
          fieldProps={{ prefix: <LockOutlined /> }}
          placeholder="密码"
          rules={[{ required: true, message: '请输入密码' }]}
        />
      </LoginForm>
    </div>
  );
};

export default LoginPage;
