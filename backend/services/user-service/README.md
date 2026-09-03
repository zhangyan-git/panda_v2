# user-service

用户服务首条业务链路使用 HTTP JSON：`POST /users` 注册用户（请求 `{ "name": "张三" }`），`GET /users/{id}` 按 ID 查询。当前不包含密码、登录或 Token。

数据库使用 PostgreSQL，启动 API 前可在项目根目录 `.env` 中配置 `DATABASE_URL`，服务会自动读取；也可以使用环境变量覆盖文件配置。启动前需执行 `migrations/001_create_users.sql`。仓库未配置 protoc 生成链路，因此本链路暂采用清晰的 HTTP JSON 路由；后续可在生成链路就绪后补充协议定义。
