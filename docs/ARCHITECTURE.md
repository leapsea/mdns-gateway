# mDNS Gateway v1 工程架构

## 1. 需求与非功能约束

业务实体是 `User`、`Session`、`Site`、`Desired State`、`Candidate` 和 `Active Release`。单网关服务家庭、实验室或小型办公局域网；第一版假设站点数量为几十个、后台并发为个位数，配置变更可以等待最多数秒的 mDNS 探测与 OpenResty 校验。配置变更串行执行，普通代理请求不经过 Go 后端。

安全边界是拥有网关主机和 OpenResty 配置的管理员。Dashboard 的站点元数据与运行状态允许访客读取；写操作、账户信息和完整站点配置通过随机内存 Session 鉴权。公开列表剔除自定义配置内容，编辑时由鉴权详情接口加载。TOTP 密文和恢复码摘要保存在 SQLite。HTTP 明文和 `/data` 主密钥需要部署在可信局域网及受限主机上。

## 2. 技术选型与分层

使用单体分层结构：`api` 负责传输、`auth` 与 `sites` 负责业务、`database` 负责持久化、`mdns` 与 `openresty` 负责外部系统。Go 标准 HTTP 服务避免额外 Web 框架；SQLite 适合单实例小规模写入；Vue 3 + TypeScript + Pinia 提供表单和状态；OpenResty 作为同镜像内的代理进程。站点服务只为 mDNS 和 OpenResty 边界定义接口。

## 3. 目录边界

完整目录见 [README](../README.md)。`backend/internal/sites` 不读取 HTTP 请求，也不运行外部命令；`backend/internal/api` 不写 OpenResty 文件；`backend/internal/database` 不做业务判断。前端只读取 API 响应与提交表单。

## 4. 核心模型与数据流

`Site` 包含域名、目标主机和端口、协议、启用状态及 `GENERATED/CUSTOM` 模式。HTTP/WebSocket 通过域名在 80 端口分流，WebSocket 配置 Upgrade 转发；SSH 使用独立的网关 TCP 监听端口和 OpenResty stream 代理。SSH 监听端口必须避开管理端口和其他启用的 SSH 站点，且不支持自定义 HTTP 配置。`SiteService.Save` 顺序为：输入校验 → mDNS 冲突探测 → 完整 Candidate 生成与验证 → SQLite Desired 提交 → 原子发布与 reload → mDNS 注册更新。失败的 Candidate 不进入 Active。删除先验证删除后的完整配置，再发布，随后删除数据库记录。

登录通过 `AuthService` 查询加密的 TOTP Secret；成功后创建进程内 Session。CLI 重置将账户标记为可用用户名重新绑定，并递增认证代数；API 每次验证 Session 时对照数据库代数，使独立 CLI 进程重置后旧会话立即失效。恢复码重置不会开启用户名绑定，验证后使用一次性令牌直接进入新二维码。认证失败记录真实客户端 IP 和事件。相同 IP 每两次失败进入下一封禁等级；连续七天无失败清零。

## 5. 可运行骨架

入口在 [main.go](../backend/cmd/mdns-gateway/main.go)，集中注入 SQLite、认证、mDNS、OpenResty 和站点服务。站点编排见 [service.go](../backend/internal/sites/service.go)，配置生命周期见 [manager.go](../backend/internal/openresty/manager.go)。测试中的 `fakeMDNS`、`fakePublisher` 和 `fakeRunner` 演示了对外部边界的依赖反转。

## 6. 防御性设计与演进

API 统一返回 `{data,error}`，公开错误码与内部错误分离；后端 JSON 日志记录请求和关键事件，认证审计写入 `auth_events`。环境变量集中在 `backend/internal/config`，默认数据卷为 `/data`。数据库只初始化当前表结构；启动时验证必需字段，旧库不兼容时明确报错，由部署者自行重新初始化。进程退出时先停止 HTTP，再关闭 mDNS、OpenResty 和 SQLite。

主要瓶颈是单 SQLite 写连接、串行配置变更和单网卡 mDNS 广播。未来如果增加大量站点或多网段支持，应首先扩展 mDNS 的逐网卡实例和站点状态聚合；不要把单实例 SQLite 直接放到共享卷上运行多副本。配置版本化已经为故障回退和状态对账留下了边界。
