# mDNS Gateway

将内网服务映射为 `media.local` 这样的地址，并通过同一进程管理 mDNS、SQLite 和 OpenResty。Web 管理界面由 Vue 3 构建，后端为 Go。站点支持 HTTP、WebSocket 和 SSH 转发。

## 架构与目录

```text
backend/cmd/mdns-gateway/       进程入口、CLI 与内嵌前端
backend/internal/api/           HTTP 路由、鉴权边界与统一响应
backend/internal/auth/          TOTP、恢复码、内存 Session、IP 封禁
backend/internal/sites/         站点领域模型和跨组件编排
backend/internal/mdns/          局域网发现、应答和冲突监测
backend/internal/openresty/     完整配置生成、候选验证、原子发布和回退
backend/internal/database/      SQLite 建库和表定义
backend/internal/config/        部署参数
build/                          本地测试镜像构建入口
web/                    Vue 3 管理界面
deploy/                 host 网络 Compose 示例
scripts/                本地验证脚本
```

站点变更通过 `API → Site Service → SQLite / mDNS / OpenResty`。`Site Service` 依赖 mDNS 和发布接口；外部操作可在测试中替换。只对真正的外部边界使用接口，避免把普通业务对象拆成大量抽象。

## 配置状态

用户保存的是 Desired State。服务先生成**全量** Candidate，调用 `openresty -t` 验证，再切换 `/data/openresty/current` 符号链接并 reload。验证失败时旧 Active 保持不变；reload 失败时把链接切回上一 Release。数据库分别记录 Desired 版本和 Active 对应版本，启动时加载已发布 Release，不会用 SQLite 的最新站点覆盖它。

首次启动建立空的 Active Release；随后用户创建站点。删除站点先验证删除后的完整配置，再发布，再删除站点数据。Release 默认保留最近五份，并保护当前和上一份。

## 本地部署

Linux Docker 主机推荐使用 `network_mode: host`，使 mDNS 组播能到达局域网。数据卷保存 `gateway.db`、`master.key` 和 OpenResty Release。本地 Compose 的 Web 后台使用 `:8082`，可通过 `GATEWAY_HTTP_PORT` 调整；直接运行程序的默认端口为 8080。HTTP/WebSocket 代理监听 `:80`，mDNS 使用 UDP 5353。SSH 站点使用创建时指定的网关 TCP 端口，需允许客户端访问该端口。例如网关端口为 2222、域名为 `shell.local` 时，使用 `ssh -p 2222 user@shell.local`。SSH 认证和主机密钥由目标 SSH 服务提供。

在 macOS Docker Desktop 上，`network_mode: host` 需要在 Docker Desktop 的 **Settings → Resources → Network → Enable host networking** 单独启用并重启。即使后台可从 Mac 的 `localhost:8082` 打开，仍需在目标局域网验证 mDNS 组播；完整局域网功能优先在原生 Linux Docker 主机验收。遇到容器内 `/health` 正常、Mac 上连接被拒绝时，在客户端运行 `sh scripts/diagnose-network.sh` 并回传输出。

```sh
docker compose -f deploy/compose.yml up --build -d
curl http://127.0.0.1:8082/health
```

打开 `http://<网关IP>:8082/` 初始化管理员。系统会显示 TOTP 二维码和只显示一次的恢复码。`master.key` 与数据库一起备份；丢失主密钥后，现有 TOTP 密文无法恢复。请限制 `/data` 的访问权限。

### Ubuntu Docker 部署与 mDNS 排查

这里使用 Ubuntu 上的 Docker Engine。`host` 模式共享主机网络，mDNS 使用真实网卡；不配置 `ports` 映射。[Docker host 网络说明](https://docs.docker.com/engine/network/drivers/host/)

在 Ubuntu 上执行 `ip -4 -br addr`，找出与客户端同一局域网的网卡 IPv4。复制 `deploy/.env.example` 为 `deploy/.env`，填写 `GATEWAY_IPV4`（例如 `192.168.50.30`）和未被占用的 `GATEWAY_HTTP_PORT`，再启动：

```sh
cd deploy
docker compose up --build -d --wait
docker compose logs --tail=30
```

启动日志中的 `mdns interface selected` 应显示真实局域网网卡和填写的地址。`media.local` 应解析到这个 Ubuntu 网关地址，再由网关代理到站点中配置的 `192.168.50.21:2030`。

在 Ubuntu 的项目根目录运行：

```sh
sh scripts/diagnose-network.sh media.local http://192.168.50.21:2030/
```

若绕过解析的代理请求返回 200，但客户端仍不能解析，检查 Ubuntu 防火墙是否允许局域网来源的 UDP 5353，并检查客户端与网关之间是否有 VLAN、访客 Wi-Fi 或 AP 隔离。HTTP 代理 TCP 80 和管理 TCP 8082 也应按需允许局域网访问。mDNS 查询在同一网络链路传播，跨网段需要额外的 mDNS 转发设施。不要仅凭 Ubuntu 上 `curl media.local` 的结果判断外部客户端是否能解析：客户端还应执行 `curl --noproxy '*' http://media.local/`；Ubuntu 客户端需要系统解析器支持 mDNS。

重置验证器：

```sh
docker exec -it <container> mdns-gateway user reset-2fa admin
```

CLI 会询问确认并使原 TOTP 失效。下次打开登录页时，系统进入“重新绑定验证器”，输入账户名即可生成新二维码；绑定完成前，知道账户名且能访问后台的人也可抢先绑定，因此应只在可信本地网络执行重置并立即完成绑定。恢复码路径在验证成功后直接显示新二维码，内部使用一次性令牌，不要求用户再次输入。

## 本地检查

在有依赖下载权限的**客户端**执行 `sh scripts/check-local.sh`。脚本运行 Go 格式检查、`go vet`、Go 测试和 Vue 构建。执行 `sh build/build-local-image.sh` 编译本地测试镜像 `mdns-gateway:local`；执行 `sh scripts/check-container.sh` 会构建镜像并检查启动、健康接口和当前 OpenResty 配置。请将失败输出回传。局域网 mDNS 冲突和真实反向代理仍需在目标网络验收。云端发布是否成功以 GitHub Actions 运行结果和 GHCR 镜像为准。

可配置项：`GATEWAY_LISTEN`、`GATEWAY_DATA_DIR`、`GATEWAY_OPENRESTY_BIN`、`GATEWAY_IPV4`、`GATEWAY_SESSION_TTL_SECONDS`、`GATEWAY_MDNS_PROBE_SECONDS`。多网卡主机应显式设置 `GATEWAY_IPV4`。

## 状态与边界

认证成功不会重置 IP 封禁历史；两次失败触发封禁，时长为 10、30、60、120、240 分钟，之后倍增。连续七天没有新的认证失败后会清零历史。封禁最长七天，以确保不存在永久封禁。Session 只保存在 Go 进程内存，重启即失效。

当前不包含 HTTPS、证书管理或全局限流。数据库不使用迁移版本号，也不自动升级旧表结构；如果现有 `gateway.db` 与当前程序不兼容，启动会明确报错。需要重新初始化时，先停止容器并自行移走旧数据库及其 WAL/SHM 文件，再启动新实例。使用相同数据卷运行两个网关实例不受支持。

## GitHub Actions 与镜像发布

推送到 `main` 后，发布流程先检查 Go 格式、运行 Go vet / 测试并构建前端，再构建 `linux/amd64` 和 `linux/arm64` 镜像。两个架构均通过容器启动、健康接口和 OpenResty 配置检查后，将同一份镜像发布至 `ghcr.io/<owner>/<repository>:latest`，并保留 `sha-<完整提交SHA>` 标签。镜像路径自动转为小写。

推送 `v*` 标签会发布对应版本镜像；稳定标签同时更新 `latest`，包含 `-` 的预发布标签只发布版本及 SHA 标签。也可在 Actions 页面手动运行；选择 `main` 时更新 `latest`，其他分支只发布 SHA 标签。Pull Request 只执行 CI 检查，不发布镜像。发布任务串行执行，并使用 GitHub Actions 构建缓存。

发布使用仓库自带的 `GITHUB_TOKEN`，仅镜像发布任务授予 `packages: write`，无需配置额外密钥。仓库需要启用 Actions 并允许该任务写入 Packages；若同名 GHCR 包已经存在，应确保仓库有写入权限。首次发布后，如需匿名拉取，请在 GitHub Package 设置中将可见性改为 Public。

现有 `deploy/compose.yml` 仍用于本地源码构建。使用发布镜像时，将 `image` 改为 `ghcr.io/<owner>/<repository>:latest` 并移除 `build`，然后在 `deploy` 目录执行 `docker compose pull && docker compose up -d`。镜像发布不会自动更新已经运行的容器。
