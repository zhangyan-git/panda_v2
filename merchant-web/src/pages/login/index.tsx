import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { LoginForm, ProFormText } from '@ant-design/pro-components';
import { history, useModel } from '@umijs/max';
import { message } from 'antd';
import { fetchCurrentUser, login } from '../../services/user';
import { clearSession, requestErrorMessage, saveTokens } from '../../services/session';

const LoginPage: React.FC = () => {
  const { setInitialState } = useModel('@@initialState');

  const handleSubmit = async (values: { username: string; password: string }) => {
    try {
      const tokens = await login(values);
      saveTokens(tokens);
      const currentUser = await fetchCurrentUser();
      await setInitialState((s) => ({ ...s, currentUser }));
      message.success('登录成功');
      history.replace('/dashboard');
    } catch (err) {
      clearSession();
      await setInitialState((s) => ({ ...s, currentUser: undefined }));
      message.error(requestErrorMessage(err, '登录失败，请检查用户名和密码'));
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
        title="Panda 商户"
        subTitle="商户管理平台"
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
