# panda_v2 开发测试集群部署清单

给公司办公室 K3s 开发测试集群用的一套 Kubernetes 清单，对应
`gitadmin/cluster-requests` 的 Issue #10。手册是
[开发者集群手册](https://git.dev.51qituan.com/gitadmin/cluster-requests/src/branch/main/docs/开发者集群手册.md)。

## 这份清单是什么，不是什么

**是**：23 个业务工作负载 + 9 个中间件 + 3 个对外入口的完整描述，
以及它们的配置、探针、资源量、持久卷和依赖关系。

**不是**：不是我们自己去 apply 的东西。按手册 §2.5 与 §6，namespace 由维护方
预置、最小 RBAC 由维护方签发，普通开发者不直接创建 Namespace。
这套清单是**提交给维护方复核**的，apply 由维护方或维护方授权的身份执行。

所以下面每一条命令都是「复核通过后才会跑的」，不是已经在跑的。

## 前置（缺一不可）

| 项 | 谁做 | 说明 |
| --- | --- | --- |
| namespace `pandaaaa` | 维护方 | `00-namespace.yaml` 是一份草案，不是让我们 apply 的 |
| 该 namespace 的最小 RBAC | 维护方 | 至少 Deployment/StatefulSet/Service/Ingress/ConfigMap/Secret/Job/PVC |
| 仓库 Actions 已启用 | 维护方 | workflow 在 `.gitea/workflows/`，runner 标签 `ubuntu-latest`（手册 §2、§4） |
| 14 个镜像推入内网 registry | CI | `.gitea/workflows/build-and-push.yml` 手动触发；也可本地按「镜像」一节构建 |
| registry 能被节点拉取 | 维护方 | 明文 HTTP 的话节点要配 insecure registry，见「待确认」第 2 条 |
| `panda-secret` | 我们自己 | 从 `02-secret.example.yaml` 改一版，**不进仓库** |
| 入口网段确认 | 维护方 | `TRUSTED_PROXY_CIDRS` / `PARTNER_TRUSTED_PROXY_CIDRS`，见「待确认」 |

## 部署顺序

顺序不是仪式感：`postgres` 要在 migrate 之前，migrate 要在所有业务服务之前，
`panda-common`/`panda-secret` 要在所有工作负载之前。文件名自带数字前缀，
`kubectl apply -f <目录>` 按文件名顺序处理，所以目录**平铺**是有意的
（`optional/` 那个子目录不会被递归读到，那正是它待在那里的原因）。

### 1. 从仓库里现成的文件生成 ConfigMap

这几个配置在仓库里已经有一份，**不在 `deploy/k8s/` 里复制第二份** ——
复制一份就多一个会漂移的地方。用 `--from-file` 现做：

```bash
cd <仓库根目录>

kubectl -n pandaaaa create configmap postgres-initdb \
  --from-file=01-databases.sql=deploy/compose/dev/initdb/01-databases.sql

kubectl -n pandaaaa create configmap tempo-config \
  --from-file=tempo.yaml=deploy/observability/tempo.yaml

# 注意 key 必须是 prometheus.yml：容器里 --config.file 指的就是这个名字
kubectl -n pandaaaa create configmap prometheus-config \
  --from-file=prometheus.yml=deploy/observability/prometheus.yml

# 注意 key 必须是 config.yaml，同上
kubectl -n pandaaaa create configmap otel-collector-config \
  --from-file=config.yaml=deploy/observability/otel-collector.yaml

kubectl -n pandaaaa create configmap grafana-datasources \
  --from-file=deploy/observability/grafana/provisioning/datasources/

kubectl -n pandaaaa create configmap grafana-dashboards \
  --from-file=deploy/observability/grafana/provisioning/dashboards/
```

`postgres-initdb` 只在**数据卷首次初始化**时执行一次。卷已存在时改了那个 SQL 不会
重跑，要手工 `createdb` —— 这一点与本仓的 compose 栈行为一致。

### 2. 凭据

`02-secret.example.yaml` 里全是占位值。真正的那份从本机一份不进版本控制的文件建：

```bash
kubectl -n pandaaaa create secret generic panda-secret \
  --from-env-file=/path/to/panda-secret.env \
  --dry-run=client -o yaml | kubectl apply -f -
```

`panda-secret.env` 的格式就是 `.env`，可以直接拿 `deploy/config/.env.example`
改一版：把里面指向 `localhost` 的地址换成下面的服务名，把密钥换成真值。

几个键的取值口径写在 `02-secret.example.yaml` 的注释里，最容易踩的两个是：

- `*_DATABASE_URL` 必须指向 `postgres:5432`。各服务只认自己那一个，变量为空时
  `config.Load` **拒绝启动**，不回落到共享默认库 —— 回落会让服务把自己的表
  建到别人的库里，而且不报错。
- `PARTNER_SECRET_KEY` 必须解出来正好 32 字节，否则 partner-service 拒绝启动。

### 3. 中间件 → 迁移 → 服务

```bash
kubectl -n pandaaaa apply -f deploy/k8s/00-namespace.yaml   # 仅当维护方授权时
kubectl -n pandaaaa apply -f deploy/k8s/01-configmap.yaml
kubectl -n pandaaaa apply -f deploy/k8s/02-secret.example.yaml   # 或上面那份真的

kubectl -n pandaaaa apply -f deploy/k8s/10-postgres.yaml
kubectl -n pandaaaa apply -f deploy/k8s/11-redis.yaml
kubectl -n pandaaaa apply -f deploy/k8s/12-rabbitmq.yaml
kubectl -n pandaaaa apply -f deploy/k8s/13-etcd.yaml
kubectl -n pandaaaa apply -f deploy/k8s/14-opensearch.yaml
kubectl -n pandaaaa apply -f deploy/k8s/15-opensearch-settings-job.yaml
kubectl -n pandaaaa apply -f deploy/k8s/16-tempo.yaml
kubectl -n pandaaaa apply -f deploy/k8s/17-prometheus.yaml
kubectl -n pandaaaa apply -f deploy/k8s/18-otel-collector.yaml
kubectl -n pandaaaa apply -f deploy/k8s/19-grafana.yaml

# 等 postgres 变成 Running/Ready 再继续
kubectl -n pandaaaa wait --for=condition=ready pod -l app=postgres --timeout=300s

# 建表。**必须在任何业务服务之前**，见 50-migrate-job.yaml 文件头的说明。
kubectl -n pandaaaa apply -f deploy/k8s/50-migrate-job.yaml
kubectl -n pandaaaa wait --for=condition=complete job/panda-migrate --timeout=600s
kubectl -n pandaaaa logs job/panda-migrate

kubectl -n pandaaaa apply -f deploy/k8s/20-user-service.yaml
# ... 21 到 30，逐个或一次全给
kubectl -n pandaaaa apply -f deploy/k8s/
```

最后一条会把 20–32、40 一起应用，这是可以的：服务起不来时探针会一直失败，
`kubectl get pods` 上一眼能看出来，不会有半截状态被藏起来。

### 4.（可选）建初始账号

`optional/seed-job.yaml` 会**覆盖**管理员密码并授予 `super_admin`，
所以它是显式可选的、不进主目录。首次验收需要能登录后台时跑一次：

```bash
# 先按文件头的命令构建 user-service-seed 镜像
kubectl -n pandaaaa apply -f deploy/k8s/optional/seed-job.yaml
kubectl -n pandaaaa logs job/panda-seed
```

## 镜像

14 个，全部从本仓库构建，构建上下文**都必须是仓库根目录**。

**正常路径是 CI，不是下面的命令**：`.gitea/workflows/build-and-push.yml`
（Gitea Actions，`workflow_dispatch` 手动触发，填要构建的 SHA）。它是构建的
唯一入口 —— 下面的命令是同一件事的本地版，用来说清构建矩阵，手工跑仅用于
排查（本机拉不到 Docker Hub 时也跑不了）。

```bash
SHA=$(git rev-parse HEAD)
# 维护方在 Issue #10 里给的内网 registry 地址。手册 §5/§6 的模板写的是
# registry.dev.51qituan.com —— 同一个 registry 的另一个地址，仓库路径
# （pandaaaa/<service>）相同。**清单里用的是这一个**，因为推送与拉取用同一个
# 字符串才不会出现「推到一个地址、kubelet 去另一个地址找」这种
# 只表现为 ImagePullBackOff 的错配。
REG=192.168.18.75:30500/pandaaaa

# 11 个 Go 服务
for s in gateway-service user-service merchant-service coffee-machine-service \
         coupon-service payment-service order-service account-service \
         lottery-service membership-service partner-service; do
  docker build -f deploy/docker/Dockerfile.backend --build-arg SERVICE=$s -t $REG/$s:$SHA .
  docker push $REG/$s:$SHA
done

# 迁移执行体（本项目独有的第 14 个，理由见 Dockerfile.migrate 文件头）
docker build -f deploy/docker/Dockerfile.migrate -t $REG/panda-migrate:$SHA .
docker push $REG/panda-migrate:$SHA

# 2 个前端
for a in admin-web merchant-web; do
  docker build -f deploy/docker/Dockerfile.web --build-arg APP=$a -t $REG/$a:$SHA .
  docker push $REG/$a:$SHA
done
```

清单里的 tag 是 `ae557f024e9dc815df77f5183d5bd0db1f2c9be9`（当前 `main`）。
**换版本要同时改两处**：各 Deployment 的 `image`，和同一文件里
`app.kubernetes.io/version` 那个 Pod 标签 —— 后者经 downward API 变成
`SERVICE_VERSION`，也就是链路里看到的版本号。

不使用 `latest`（手册 §5）。基础镜像全部是 Dockerfile 里的 `ARG`
（`GO_IMAGE` / `RUNTIME_IMAGE` / `NODE_IMAGE` / `NGINX_IMAGE`），
开发机拉不到 Docker Hub 时可以通过 `--build-arg` 指向内网镜像。

## 资源与持久卷

清单里的合计（由 `kubectl` 之外的脚本从 YAML 直接算出来，不是估的）：

| 口径 | CPU | 内存 |
| --- | --- | --- |
| requests 合计 | 4.12 | 8.25 Gi |
| limits 合计 | 12.50 | 20.50 Gi |

与 Issue #10 里申请的量一致。这个合计**不含** `optional/seed-job.yaml`
（它是一次性 Job，多 0.10 CPU / 0.12 Gi 的 requests，0.50 / 0.50 的 limits）。

存储 8 个 PVC 合计 **95 Gi**：

| PVC | 容量 | 说明 |
| --- | --- | --- |
| opensearch | 30Gi | 全部服务的日志都在这里 |
| postgres | 25Gi | 十个库 |
| tempo | 10Gi | 链路保留 24h（见 tempo.yaml 的 block_retention） |
| prometheus | 10Gi | |
| redis / rabbitmq / etcd / grafana | 各 5Gi | |

`local-path` 的 PV 带节点亲和性（手册 §6）：Pod 第一次调度到哪个节点，卷就钉在
哪个节点，换节点会 Pending。**这里没有做节点亲和**，是让调度器自己决定 ——
手工把八个卷钉到同一个节点，那个节点一坏就是全部数据一起没。
`postgres` 与 `opensearch` 是真正该关心这一点的两个，其余六个卷的内容都可以重建。

## 部署之后怎么验证

```bash
kubectl -n pandaaaa get pods -o wide
kubectl -n pandaaaa get ingress,svc
kubectl -n pandaaaa get pvc

# 三个入口是否真的通
curl -sS -o /dev/null -w '%{http_code}\n' https://admin.pandaaaa.51qituan.com/
curl -sS -o /dev/null -w '%{http_code}\n' https://merchant.pandaaaa.51qituan.com/
curl -sS -o /dev/null -w '%{http_code}\n' https://api.pandaaaa.51qituan.com/livez

# 网关自己的健康，绕开前面所有层
kubectl -n pandaaaa port-forward svc/gateway-service 8080:8080 &
curl -sS http://127.0.0.1:8080/readyz
```

看日志与链路的面板（**grafana 没有 Ingress**，只能这样进去）：

```bash
kubectl -n pandaaaa port-forward svc/grafana 3000:3000
# 浏览器打开 http://127.0.0.1:3000 ，匿名可看（Viewer）
```

RabbitMQ 管理界面同理：

```bash
kubectl -n pandaaaa port-forward svc/rabbitmq 15672:15672
```

## 待确认（apply 之前必须有答案）

1. **入口网段。** `gateway-service` 的 `TRUSTED_PROXY_CIDRS` 和 `partner-service` 的
   `PARTNER_TRUSTED_PROXY_CIDRS` 现在都填 `10.42.0.0/16`（k3s 默认 pod CIDR）。
   集群改过 `cluster-cidr` 的话这两个值是错的，而**错了不会报错**：
   网关会按入口地址给全站分一个限流桶（一个人刷，所有人一起 429），
   合作方 IP 白名单会把所有调用判成 `401 IP_NOT_ALLOWED`，看着像对方填错了 IP。
2. **registry 是不是 HTTP（insecure）。** 已按维护方给的
   `192.168.18.75:30500/pandaaaa` 统一了推送与拉取。剩下的问题是 containerd
   能不能直接拉它：如果它是明文 HTTP，节点上的 `/etc/rancher/k3s/registries.yaml`
   必须把这个地址配成 `insecure: true`，否则拉取会以
   `http: server gave HTTP response to HTTPS client` 失败 —— 报错在
   `kubectl describe pod` 的 Events 里，不在镜像构建那侧，所以容易被当成
   「镜像没推上去」。这个配置在节点上，得由维护方确认。
3. **`PAYMENT_NOTIFY_BASE_URL`。** 现在填 `https://api.pandaaaa.51qituan.com`。
   它必须是一个**渠道能从公网到达**的地址（渠道在集群外面，解析不了服务名）。
   按手册 §1 的拓扑（公网 HTTPS → 云端 Nginx → WireGuard → Traefik）这三个域名
   是公网可达的，所以这个值是对的；如果实际只有办公室能到，这条链路先不验。

## 已知缺口（有意没做的）

- **没有 NetworkPolicy。** 二十个工作负载互相可达。开发测试集群，先不加;
  要加的话至少应该把 postgres/redis/rabbitmq/etcd 限制成只接受本 namespace 内、
  且只用它们的那些服务的流量。
- **没有 PodDisruptionBudget、没有 HPA。** 全部 `replicas: 1`。
  单副本 + `local-path` 意味着这些 Pod 换节点会 Pending，本来也谈不上可用性。
- **没有 resources 之外的驱逐保护。** `local-path` 的容量是节点磁盘，
  写满的表现是 Pod 被驱逐而不是写入失败。
- **Grafana 匿名 Viewer 是开着的。** 前提是它没有 Ingress、只能 port-forward
  进去。给它加 Ingress 或把 `19-grafana.yaml` 那段改成要登录，两件事要一起做。
- **备份没接。** `deploy/prod/` 那套备份脚本是给生产写的，
  这个集群里的 postgres 没有接 WAL 归档，也没有定时 `pg_dump`。
  开发测试数据丢失是可接受的，但**别把这里当唯一一份数据的地方**。

## 本次没有做的事

- **没有跑过 `kubectl apply --dry-run`。** 本机没有集群上下文，
  `kubectl` 取不到 openapi，`--dry-run=client` 也会退化成「连不上」。
  已经做过的是：YAML 全部解析通过（含多文档）、
  每一处 `secretKeyRef` 的键都在 `02-secret.example.yaml` 里存在、
  每一处 `configMapRef` 与卷挂载的 ConfigMap 都在上面第 1 步的生成清单里。
  **真正的 schema 校验要在有集群上下文的地方做一次。**
- **镜像一个都还没构建。** 清单里的 tag 现在指向不存在的镜像，
  Pod 会停在 `ImagePullBackOff` —— 那是预期状态，不是配错了。
