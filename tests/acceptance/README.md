# tests/acceptance

验收用例的可执行形式。目录名等于设计仓 `03-acceptance.md` 的用例编号，一个目录一个用例；
`harness` 是用例共用的装配与断言包，不属于任何用例。

设计依据：`tsic/kos-design@05bf60a`。编号规则见 `kosd/adr/ADR-036-design-repo-project-layout.md`：
核心用例用 `KOSD-TC-x`，项目自有用例用项目前缀，编号全局唯一。

## 旧编号到设计编号的映射

旧目录的全局编号 `TC-x` 在设计仓按项目重编号后落在下表的新目录；多数与旧编号同号，
同号条目分属多个项目时按用例内容归属。

两处例外来自设计锚点之后的一次移动：节点失联的时间轴整条从 `kos-node` 移入 `kosd`，
旧编号 84、85 的用例随之改归 `KOSD-AC-74` 下的 `KOSD-TC-161`、`KOSD-TC-162`；
锚点里的 `NODE-TC-84`、`NODE-TC-85` 在移动后不再存在，映射取移动后的条目。

| 旧目录 | 新目录 | 设计文件 | 条目 |
|---|---|---|---|
| `TC-1/` | `KOSD-TC-1/` | `kosd/03-acceptance.md` | KOSD-TC-1 产品安装与内核 kind 集合 |
| `TC-2/` | `KOSD-TC-2/` | `kosd/03-acceptance.md` | KOSD-TC-2 能力基线不因产品改变 |
| `TC-3/` | `KOSD-TC-3/` | `kosd/03-acceptance.md` | KOSD-TC-3 第一方与第三方同类贡献一致 |
| `TC-4/` | `KOSD-TC-4/` | `kosd/03-acceptance.md` | KOSD-TC-4 声明完整 |
| `TC-5/` | `KOSD-TC-5/` | `kosd/03-acceptance.md` | KOSD-TC-5 未声明与未安装的贡献不可见 |
| `TC-6/` | `KOSD-TC-6/` | `kosd/03-acceptance.md` | KOSD-TC-6 依赖缺失与失败隔离 |
| `TC-7/` | `KOSD-TC-7/` | `kosd/03-acceptance.md` | KOSD-TC-7 设置项出现与修改 |
| `TC-8/` | `KOSD-TC-8/` | `kosd/03-acceptance.md` | KOSD-TC-8 停用后设置项消失 |
| `TC-9/` | `APPSTORE-TC-9/` | `kos-appstore/03-acceptance.md` | APPSTORE-TC-9 目录来源同步与摘要 |
| `TC-10/` | `APPSTORE-TC-10/` | `kos-appstore/03-acceptance.md` | APPSTORE-TC-10 来源移除 |
| `TC-11/` | `KOSD-TC-11/` | `kosd/03-acceptance.md` | KOSD-TC-11 发现不自动接管 |
| `TC-12/` | `DIAG-TC-12/` | `kos-diagnostics/03-acceptance.md` | DIAG-TC-12 证据链 |
| `TC-13/` | `DIAG-TC-13/` | `kos-diagnostics/03-acceptance.md` | DIAG-TC-13 适配器不可用 |
| `TC-14/` | `DASH-TC-14/` | `kos-dashboard/03-acceptance.md` | DASH-TC-14 偏好跨浏览器 |
| `TC-15/` | `DASH-TC-15/` | `kos-dashboard/03-acceptance.md` | DASH-TC-15 偏好保留语义 |
| `TC-16/` | `KOSD-TC-16/` | `kosd/03-acceptance.md` | KOSD-TC-16 未标注对象只读 |
| `TC-17/` | `KOSD-TC-17/` | `kosd/03-acceptance.md` | KOSD-TC-17 接管、释放与放弃 |
| `TC-18/` | `KOSD-TC-18/` | `kosd/03-acceptance.md` | KOSD-TC-18 父对象不写子对象 spec |
| `TC-19/` | `KOSD-TC-19/` | `kosd/03-acceptance.md` | KOSD-TC-19 标记载体 |
| `TC-23/` | `KOSD-TC-23/` | `kosd/03-acceptance.md` | KOSD-TC-23 内置定义安装 |
| `TC-24/` | `KOSD-TC-24/` | `kosd/03-acceptance.md` | KOSD-TC-24 并发写入 |
| `TC-25/` | `KOSD-TC-25/` | `kosd/03-acceptance.md` | KOSD-TC-25 完成条件三值 |
| `TC-26/` | `KOSD-TC-26/` | `kosd/03-acceptance.md` | KOSD-TC-26 重复调和幂等 |
| `TC-27/` | `KOSD-TC-27/` | `kosd/03-acceptance.md` | KOSD-TC-27 陈旧标记 |
| `TC-28/` | `KOSD-TC-28/` | `kosd/03-acceptance.md` | KOSD-TC-28 写请求幂等 |
| `TC-29/` | `KOSD-TC-29/` | `kosd/03-acceptance.md` | KOSD-TC-29 错误响应与断线 |
| `TC-30/` | `KOSD-TC-30/` | `kosd/03-acceptance.md` | KOSD-TC-30 可见性谓词 |
| `TC-31/` | `KOSD-TC-31/` | `kosd/03-acceptance.md` | KOSD-TC-31 角色写权限 |
| `TC-32/` | `KOSD-TC-32/` | `kosd/03-acceptance.md` | KOSD-TC-32 角色不改变入口可达性 |
| `TC-33/` | `KOSD-TC-33/` | `kosd/03-acceptance.md` | KOSD-TC-33 所有者转移 |
| `TC-34/` | `KOSD-TC-34/` | `kosd/03-acceptance.md` | KOSD-TC-34 扩展页面权限 |
| `TC-35/` | `KOSD-TC-35/` | `kosd/03-acceptance.md` | KOSD-TC-35 计划与执行一致 |
| `TC-36/` | `KOSD-TC-36/` | `kosd/03-acceptance.md` | KOSD-TC-36 审计记录 |
| `TC-37/` | `KOSD-TC-37/` | `kosd/03-acceptance.md` | KOSD-TC-37 凭据边界 |
| `TC-38/` | `KOSD-TC-38/` | `kosd/03-acceptance.md` | KOSD-TC-38 内置 Provider 注册 |
| `TC-39/` | `KOSD-TC-39/` | `kosd/03-acceptance.md` | KOSD-TC-39 部分 conformance 失败 |
| `TC-40/` | `KOSD-TC-40/` | `kosd/03-acceptance.md` | KOSD-TC-40 仅自报 |
| `TC-41/` | `KOSD-TC-41/` | `kosd/03-acceptance.md` | KOSD-TC-41 删除受阻 |
| `TC-42/` | `KOSD-TC-42/` | `kosd/03-acceptance.md` | KOSD-TC-42 能力撤回 |
| `TC-43/` | `KOSD-TC-43/` | `kosd/03-acceptance.md` | KOSD-TC-43 最小 Provider |
| `TC-44/` | `KOSD-TC-44/` | `kosd/03-acceptance.md` | KOSD-TC-44 新增 Provider 无需改 KOS |
| `TC-45/` | `KOSD-TC-45/` | `kosd/03-acceptance.md` | KOSD-TC-45 翻译稳定性 |
| `TC-46/` | `KOSD-TC-46/` | `kosd/03-acceptance.md` | KOSD-TC-46 私有配置的字段边界 |
| `TC-47/` | `KOSD-TC-47/` | `kosd/03-acceptance.md` | KOSD-TC-47 未知注解无行为 |
| `TC-48/` | `KOSD-TC-48/` | `kosd/03-acceptance.md` | KOSD-TC-48 双后端安装 |
| `TC-49/` | `KOSD-TC-49/` | `kosd/03-acceptance.md` | KOSD-TC-49 包声明存储类被拒 |
| `TC-50/` | `UPDATER-TC-50/` | `kos-updater/03-acceptance.md` | UPDATER-TC-50 正常升级 |
| `TC-51/` | `UPDATER-TC-51/` | `kos-updater/03-acceptance.md` | UPDATER-TC-51 钩子失败回滚 |
| `TC-52/` | `UPDATER-TC-52/` | `kos-updater/03-acceptance.md` | UPDATER-TC-52 不可逆声明 |
| `TC-53/` | `UPDATER-TC-53/` | `kos-updater/03-acceptance.md` | UPDATER-TC-53 外部改动暂停调和 |
| `TC-54/` | `KOSD-TC-54/` | `kosd/03-acceptance.md` | KOSD-TC-54 阶段推进与 Pending |
| `TC-55/` | `KOSD-TC-55/` | `kosd/03-acceptance.md` | KOSD-TC-55 重试预算与 Failed 终止 |
| `TC-56/` | `KOSD-TC-56/` | `kosd/03-acceptance.md` | KOSD-TC-56 卸载卡死与 orphan |
| `TC-57/` | `APPSTORE-TC-57/` | `kos-appstore/03-acceptance.md` | APPSTORE-TC-57 缺依赖与越界参数 |
| `TC-58/` | `APPSTORE-TC-58/` | `kos-appstore/03-acceptance.md` | APPSTORE-TC-58 敏感参数 |
| `TC-59/` | `DASH-TC-59/` | `kos-dashboard/03-acceptance.md` | DASH-TC-59 删除桌面条目不卸载 |
| `TC-60/` | `DASH-TC-60/` | `kos-dashboard/03-acceptance.md` | DASH-TC-60 下游改名后条目保持 |
| `TC-61/` | `DASH-TC-61/` | `kos-dashboard/03-acceptance.md` | DASH-TC-61 关闭全部窗口 |
| `TC-62/` | `SETTINGS-TC-62/` | `kos-settings/03-acceptance.md` | SETTINGS-TC-62 集群与节点设置的项与入口 |
| `TC-63/` | `KOSD-TC-63/` | `kosd/03-acceptance.md` | KOSD-TC-63 设置写者唯一 |
| `TC-64/` | `STORAGE-TC-64/` | `kos-storage/03-acceptance.md` | STORAGE-TC-64 purge 令牌 |
| `TC-65/` | `STORAGE-TC-65/` | `kos-storage/03-acceptance.md` | STORAGE-TC-65 删除应用保留数据 |
| `TC-66/` | `STORAGE-TC-66/` | `kos-storage/03-acceptance.md` | STORAGE-TC-66 成功迁移 |
| `TC-67/` | `STORAGE-TC-67/` | `kos-storage/03-acceptance.md` | STORAGE-TC-67 阶段失败回滚 |
| `TC-68/` | `STORAGE-TC-68/` | `kos-storage/03-acceptance.md` | STORAGE-TC-68 清理不删除源卷 |
| `TC-69/` | `BACKUP-TC-69/` | `kos-backup/03-acceptance.md` | BACKUP-TC-69 恢复到新实例 |
| `TC-70/` | `BACKUP-TC-70/` | `kos-backup/03-acceptance.md` | BACKUP-TC-70 失败域检查 |
| `TC-71/` | `BACKUP-TC-71/` | `kos-backup/03-acceptance.md` | BACKUP-TC-71 演练失败 |
| `TC-72/` | `KOSD-TC-72/` | `kosd/03-acceptance.md` | KOSD-TC-72 数据库恢复 |
| `TC-73/` | `NET-TC-73/` | `kos-network/03-acceptance.md` | NET-TC-73 键互斥与释放 |
| `TC-74/` | `NET-TC-74/` | `kos-network/03-acceptance.md` | NET-TC-74 目标应用不可变 |
| `TC-75/` | `NET-TC-75/` | `kos-network/03-acceptance.md` | NET-TC-75 绕过接口的重复 |
| `TC-76/` | `NET-TC-76/` | `kos-network/03-acceptance.md` | NET-TC-76 路径入口的正反路径 |
| `TC-77/` | `NET-TC-77/` | `kos-network/03-acceptance.md` | NET-TC-77 L4 端口分配 |
| `TC-78/` | `NET-TC-78/` | `kos-network/03-acceptance.md` | NET-TC-78 多播入口 |
| `TC-79/` | `NET-TC-79/` | `kos-network/03-acceptance.md` | NET-TC-79 删除被依赖组件 |
| `TC-80/` | `NET-TC-80/` | `kos-network/03-acceptance.md` | NET-TC-80 证书状态与告警 |
| `TC-81/` | `NODE-TC-81/` | `kos-node/03-acceptance.md` | NODE-TC-81 越界写入；可执行目录已迁出至 `tsic404/kos-node` |
| `TC-82/` | `NODE-TC-82/` | `kos-node/03-acceptance.md` | NODE-TC-82 陈旧与回收；可执行目录已迁出至 `tsic404/kos-node` |
| `TC-83/` | `NODE-TC-83/` | `kos-node/03-acceptance.md` | NODE-TC-83 占用状态；可执行目录已迁出至 `tsic404/kos-node` |
| `TC-84/` | `KOSD-TC-161/` | `kosd/03-acceptance.md` | KOSD-TC-161 失联与恢复 |
| `TC-85/` | `KOSD-TC-162/` | `kosd/03-acceptance.md` | KOSD-TC-162 单节点退化 |
| `TC-86/` | `KOSD-TC-86/` | `kosd/03-acceptance.md` | KOSD-TC-86 只读与陈旧 |
| `TC-87/` | `KOSD-TC-87/` | `kosd/03-acceptance.md` | KOSD-TC-87 控制面重启不重放 |
| `TC-88/` | `KOSD-TC-88/` | `kosd/03-acceptance.md` | KOSD-TC-88 组件与存储故障 |
| `TC-89/` | `DIAG-TC-89/` | `kos-diagnostics/03-acceptance.md` | DIAG-TC-89 诊断包内容 |
| `TC-91/` | `ADM-TC-91/` | `kosadm/03-acceptance.md` | ADM-TC-91 卸载遗留清单 |
| `TC-92/` | `ADM-TC-92/` | `kosadm/03-acceptance.md` | ADM-TC-92 救援只读 |
| `TC-93/` | `ADM-TC-93/` | `kosadm/03-acceptance.md` | ADM-TC-93 单节点承诺 |
| `TC-95/` | `ADM-TC-95/` | `kosadm/03-acceptance.md` | ADM-TC-95 升级预检与恢复 |
| `TC-96/` | `ADM-TC-96/` | `kosadm/03-acceptance.md` | ADM-TC-96 无强制驱逐 |
| `TC-97/` | `KOSD-TC-97/` | `kosd/03-acceptance.md` | KOSD-TC-97 预算复测 |
| `TC-98/` | `KOSD-TC-98/` | `kosd/03-acceptance.md` | KOSD-TC-98 降级触发与恢复 |
| `TC-99/` | `KOSD-TC-99/` | `kosd/03-acceptance.md` | KOSD-TC-99 降级保护不可测量 |
| `TC-100/` | `KOSD-TC-100/` | `kosd/03-acceptance.md` | KOSD-TC-100 九页投影 |
| `TC-102/` | `KOSD-TC-102/` | `kosd/03-acceptance.md` | KOSD-TC-102 浏览器凭据 |
| `TC-103/` | `KOSD-TC-103/` | `kosd/03-acceptance.md` | KOSD-TC-103 三页投影 |
| `TC-104/` | `KOSD-TC-104/` | `kosd/03-acceptance.md` | KOSD-TC-104 双版本字段一致 |
| `TC-105/` | `KOSD-TC-105/` | `kosd/03-acceptance.md` | KOSD-TC-105 无 webhook |
| `TC-106/` | `KOSD-TC-106/` | `kosd/03-acceptance.md` | KOSD-TC-106 虚拟机管理器安装与使用 |
| `TC-107/` | `KOSD-TC-107/` | `kosd/03-acceptance.md` | KOSD-TC-107 跨对象引用与删除保护 |
| `TC-108/` | `KOSD-TC-108/` | `kosd/03-acceptance.md` | KOSD-TC-108 扩展能力与验证 |
| `TC-109/` | `KOSD-TC-109/` | `kosd/03-acceptance.md` | KOSD-TC-109 扩展控制器越界写入 |
| `TC-110/` | `KOSD-TC-110/` | `kosd/03-acceptance.md` | KOSD-TC-110 扩展停用与重启 |
| `TC-111/` | `KOSD-TC-111/` | `kosd/03-acceptance.md` | KOSD-TC-111 扩展卸载与数据保留 |
| `TC-112/` | `KOSD-TC-112/` | `kosd/03-acceptance.md` | KOSD-TC-112 不兼容版本安装被拒绝 |
| `TC-113/` | `KOSD-TC-113/` | `kosd/03-acceptance.md` | KOSD-TC-113 升级预检与扩展依赖 |
| `TC-114/` | `KOSD-TC-114/` | `kosd/03-acceptance.md` | KOSD-TC-114 多产品并列安装 |
| `TC-116/` | `KOSD-TC-116/` | `kosd/03-acceptance.md` | KOSD-TC-116 漂移策略 |
| `TC-117/` | `KOSD-TC-117/` | `kosd/03-acceptance.md` | KOSD-TC-117 登录、锁定与退出 |
| `TC-118/` | `KOSD-TC-118/` | `kosd/03-acceptance.md` | KOSD-TC-118 角色变更、改口令与停用 |
| `TC-119/` | `KOSD-TC-119/` | `kosd/03-acceptance.md` | KOSD-TC-119 日志隔离与截断 |
| `TC-120/` | `KOSD-TC-120/` | `kosd/03-acceptance.md` | KOSD-TC-120 集群不可达与事件窗口 |
| `TC-121/` | `KOSD-TC-121/` | `kosd/03-acceptance.md` | KOSD-TC-121 维护前置与 node-local 保护 |
| `TC-122/` | `KOSD-TC-122/` | `kosd/03-acceptance.md` | KOSD-TC-122 绕过控制面的驱逐 |
| `TC-123/` | `KOSD-TC-123/` | `kosd/03-acceptance.md` | KOSD-TC-123 停止与启动 |
| `TC-124/` | `KOSD-TC-124/` | `kosd/03-acceptance.md` | KOSD-TC-124 重启、扩缩容与并发操作 |
| `TC-125/` | `KOSD-TC-125/` | `kosd/03-acceptance.md` | KOSD-TC-125 默认与覆盖 |
| `TC-126/` | `KOSD-TC-126/` | `kosd/03-acceptance.md` | KOSD-TC-126 非法资源组合 |
| `TC-127/` | `KOSD-TC-127/` | `kosd/03-acceptance.md` | KOSD-TC-127 停止期间写副本数再启动 |
| `TC-131/` | `NODE-TC-131/` | `kos-node/03-acceptance.md` | NODE-TC-131 重复序列号只保留最新；可执行目录已迁出至 `tsic404/kos-node` |
| `TC-132/` | `KOSD-TC-132/` | `kosd/03-acceptance.md` | KOSD-TC-132 删除确认的形态与拒绝后果 |
| `TC-133/` | `KOSD-TC-133/` | `kosd/03-acceptance.md` | KOSD-TC-133 集群级需求与节点级能力 |
| `TC-134/` | `KOSD-TC-134/` | `kosd/03-acceptance.md` | KOSD-TC-134 作用域取值与越界登记 |
| `TC-135/` | `SETTINGS-TC-135/` | `kos-settings/03-acceptance.md` | SETTINGS-TC-135 资源占用的四种状态 |
| `TC-136/` | `SETTINGS-TC-136/` | `kos-settings/03-acceptance.md` | SETTINGS-TC-136 维护窗口策略项的读写 |
