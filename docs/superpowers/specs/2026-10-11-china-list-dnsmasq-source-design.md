# 中国域名分流规则源切换到 dnsmasq-china-list —— 设计规范

- 日期：2026-10-11
- 状态：待用户复核
- 前置：`2026-09-25-mosdns-dnscrypt-cdn-ech-design.md`（国内外分流两条 sequence 路径）
- 涉及组件：`internal/rules`、`cmd/mosdns-cdnctl`、`packaging/debian/postinst`、`internal/mosdnsconfig`（仅测试）、`installer/tests`

## 1. 背景

当前 `configs/cn-domains.txt` 来自 `v2fly/domain-list-community` 的 `data/cn`，解析为 8,363 条 MOSDNS 表达式。该源已审核、已 pin、自动刷新与防塌缩护栏均已就位。

本次变更把**主源**换成 `felixonmars/dnsmasq-china-list`，覆盖从 8,363 条增至约 111,000 条。v2fly 不再是主源，但**保留为回退**，并继续贡献 dnsmasq 语法无法表达的两类规则。

变更的动机是覆盖面，不是机制。MOSDNS 侧的路由图、插件、运行时装载方式**一律不变**。

## 2. 已实测的源事实

以下全部在 2026-10-11 对上游 `master` 分支实测得到，不是记忆。

### 2.1 仓库内的规则文件

| 文件 | 指令 | 规则数 | 本项目采用 |
|---|---|---:|---|
| `accelerated-domains.china.conf` | `server=/<域名>/<IPv4>` | 111,011 | 是（主表） |
| `google.china.conf` | `server=/<域名>/<IPv4>` | 112 | 是 |
| `apple.china.conf` | `server=/<域名>/<IPv4>` | 165 | 是 |
| `bogus-nxdomain.china.conf` | `bogus-nxdomain=<IP>` | 165 | **否**，见 §9 |

三份 `server=` 文件的规则行合计 111,288 行，**字节精确去重后为 111,287 个唯一域名**（三文件之间仅 1 个重复）。源数据中不含大写字母，大小写折叠不合并任何条目。

其余文件不是路由规则：`removed-cdn.txt` 是上游主动移除的域名、`cdn-testlist.txt` 是上游自测域名、`ns-blacklist.txt` / `ns-whitelist.txt` 服务于加速域名的 IP 集合变体，`Makefile`、`*.rb`、`install.sh` 是上游工具链。全部不采用。

### 2.2 IP 一律是同一个

三份文件中每一行规则的地址都是 `114.114.114.114`，无一例外。

**这个地址是上游的一个决定，不是语法占位符。** dnsmasq 的 `server=` 语法确实要求带一个地址，但上游选择的是 114DNS——它在说"这些域名请交给 114DNS 回答"。这是一个真实的、属于上游的选择。

**本项目不接受这个选择。** 上游对"用什么解析器回答国内域名"没有任何发言权：国内分支只应到达 DHCP 下发的地址。

这不是本次新增的让步，而是本项目早就写死并已在扫描的约束：

- `internal/mosdnsconfig/render.go` 的 `chinesePublicDNSAddresses` 列表（含 `114.114.114.114`，注释标注 114DNS）对**渲染完成的配置文档**做扫描，命中即失败；
- `installer/tests/test_package.py` 的 `FORBIDDEN_ADDRESSES` 同样包含它，其注释把原则写得很直白——*"A resolver ADDRESS is refused EVERYWHERE, the China domain list included. An address in a document is an upstream somebody would be asked, and the domestic branch of the routing document reaches exactly one set of upstreams: the addresses the DHCP lease published."*

因此本次转换器**只取域名，丢弃地址**。这个地址不进入发布列表、source-lock、日志或任何测试夹具；国内域名照旧走 `dhcp_forward`，即 DHCP 租约发布的地址。

**不需要**任何 DNS 应答改写，`google.china.conf` 的 112 条规则也不需要特殊处理——它们与主表走同一条国内路径，不因来自 google 文件而有别。

### 2.3 停用记录

`accelerated-domains.china.conf` 有 6 行形如 `#server=/ax1x.com/114.114.114.114 # Disabled: <链接>` 的停用记录，每行带一个上游给出的原因链接。

## 3. 目标列表

发布列表 = dnsmasq 主源 **111,287** 条 `domain:` + v2fly 补充 **70** 条 = **111,357** 条表达式。

补充的 70 条是 v2fly `data/cn` 中全部 `regexp:`（3 条）与 `full:`（67 条）。实测这 70 条与 dnsmasq 的 `domain:` 规则**零重叠**，并集恰为两者相加。

**为什么必须保留**：`server=/<域名>/` 的语义是"该域名及其所有子域"，等价于 `domain:`，没有表达精确匹配或正则的方式。丢弃这 70 条会丢掉真实能力——例如

```
regexp:.+\.awsdns-cn-[0-9][0-9]\.(biz|com|net|top)$
regexp:.+\.awsdns-cn-[0-9][a-e0-9]\.cn$
```

这两条覆盖 AWS 中国 CDN 的边缘域名，是会被真实查询命中的。

## 4. 转换规则

新增 `internal/rules/chinaconf.go`。输入是主源归档中三个 entry 的原始字节。

**解析**：逐行处理，行首到第一个 `#` 的部分截断（与 MOSDNS 自身 `utils.RemoveComment` 同义），去空白后必须**完整**匹配

```
server=/<域名>/<IPv4>
```

不匹配即转换失败。不猜测、不跳过、不"尽力而为地"取域名部分——延续 `convert.go` 既有的 "nothing here guesses" 立场：一个形状不认识的行意味着我们对上游的理解有缺口，而带着缺口发布的列表是一个洞。

**产出**：每个域名经 `mosdnsdomain.NormalizeDomain` 后写成 `domain:<域名>`。**地址部分被丢弃，绝不进入任何产物。**

解析器仍要求行里有一个合法的 IPv4 才算格式正确——这是"不猜测"的一部分：上游若改了格式，我们应当失败，而不是取走域名那一半继续。但**读到之后不使用**。§2.2 的既有门禁（`chinesePublicDNSAddresses` 与 `FORBIDDEN_ADDRESSES`）已经覆盖这个地址；本次转换器不得让它们失败。

**停用记录**：§2.3 的 6 行降级为注释后保留，但写成

```
# disabled upstream: <原因链接>
```

只保留链接，不保留 `server=/<域名>/<地址>` 那一段。原样保留会把 `114.114.114.114` 写进发布列表，违反既有门禁 `test_the_china_list_is_a_list_of_names_and_never_of_addresses`；整行丢弃则丢掉上游的取舍记录。MOSDNS 在 `#` 处截断（`LoadFromTextReader` → `utils.RemoveComment`），注释在 domain_set 文件中合法。

**去重**：以渲染后的表达式为准，与 `convert.go` 现有做法一致（两条只差属性的规则会落到同一个表达式上）。

**空结果**：解析出 0 条即失败。空的中国集合会把每一个查询静默送往境外分支。

## 5. source-lock 升到 schema v2

发布列表现在同时来自两个仓库，扁平的 v1 lock 记不下。v2：

```json
{
  "schema_version": 2,
  "primary": {
    "repository": "felixonmars/dnsmasq-china-list",
    "commit": "<40 位十六进制>",
    "sha256": "<归档摘要>",
    "entries": ["accelerated-domains.china.conf", "google.china.conf", "apple.china.conf"]
  },
  "supplement": {
    "repository": "v2fly/domain-list-community",
    "commit": "a5731758ed6bc9620b0e146ed24b07b9131893cf",
    "sha256": "<归档摘要>",
    "entry": "data/cn",
    "kinds": ["full", "regexp"]
  },
  "list_sha256": "<发布列表摘要>"
}
```

`primary` 恒为"实际产出这份列表的源"，`supplement` 可选。回退发布时 `primary` 就是 v2fly、`supplement` 省略——**同一形状覆盖两种情况，不需要第二套文档**。

**v1 lock 一律拒绝**，错误信息里给出重新 pin 的命令。把 v1 悄悄解读成 v2，会让一份 lock 读起来完整而含义已变。

`entries` 是数组而非单个 `entry`：主源要转换三个文件，一个字符串表达不了。

## 6. 布局

**随包**（`/usr/share/mosdns-router/`）：

```
cn-domains.txt              转换后的主源列表
source-lock.json            v2 lock
cn-domains-fallback.txt     v2fly 完整列表
source-lock-fallback.json   v2 lock
```

**运行时**（`/var/lib/mosdns/lists/`）同名四个文件。

`cn-domains.txt` 是 mosdns **唯一**读取的活动列表。`internal/mosdnsconfig` 与 `configs/mosdns.yaml` 的 `domain_set` **不改**——仍然只指向一个文件。这是刻意的：回退在发布期决策，换来运行时零改动、零新增路由分支、零第二个 domain_set。

## 7. 刷新决策

`mosdns-cdnctl update-lists`（`--pin-remote` 与 `--automatic` 共用）：

1. 按 commit 拉主源归档，校验归档摘要，转换三个 entry，并入补充的 70 条；
2. 护栏按源分别判定：主源条数对比上次发布的**主源**条数（下限系数 0.95 不变），补充条数对比 pin 住的补充条数，转换失败即拒绝；
3. 成功 → 原子发布主源 pair；
4. 任何失败 → **保留最后一次好结果**；仅当本机从未有过主源列表（冷启动、离线安装）→ 发布回退 pair。

**现有 0.95 阈值恰好守住这条线**：111,357 → 8,363 是 92.5% 跌幅，一次失败的刷新**永远无法**把回退列表当作"规则变少了"发布出去。回退只能由第 4 步那条独立分支产生。这是 `minimumRetained` 已经具备的性质，本次只是它第一次真正承担区分两个源的职责。

## 8. 实测：13 倍规模不构成阻碍

在 2 核开发机上，用 MOSDNS v5.3.4 自己的加载路径（`plugin/data_provider/domain_set/domain_set.go` 调 `domain.LoadFromTextReader` 进 `domain.NewDomainMixMatcher()`）实测，字节精确列表，加载耗时取 5 次中位：

| 指标 | v2fly 现状（8,363） | 本方案（111,357） |
|---|---:|---:|
| 加载耗时 | 4.2 – 6.5 ms | **100.1 – 101.3 ms** |
| 常驻堆 | 0.73 MiB | **8.36 – 8.44 MiB** |
| 匹配命中 | 145 – 174 ns | 171 – 366 ns |
| 匹配未命中 | 587 – 733 ns | 588 – 842 ns |

- **加载**：约 +95 ms。集成测试的启动预算是 `readinessTimeout = 30s`，100 ms 是它的 **0.33%**。
- **内存**：约 +7.7 MiB。Go 守护进程的正常量级。
- **查找**：两组的区间**重叠**，测不出系统差异。原因见 §8.1。

### 8.1 为什么查找不随规则数变慢

`SubDomainMatcher` 是**反向标签 trie**（`labelNode` 树，按 DNS label 下行），`Match` 逐标签下行、遇到缺失的子节点立即 `break`。查找代价是 **O(域名标签数)，与规则总数无关**。13 倍规则数不会拖慢查找。

早期一次测量显示未命中 +52%，**该结果复现不了**，是这台 2 核机器的亚微秒级噪声。若后续再见到同类"结论"，先怀疑它是否站得住。

### 8.2 端到端证据

把真实列表换进 `configs/cn-domains.txt`，用**真实路由器进程 + 真实 MOSDNS 配置**跑完整集成套件：

```
PASS
ok  mosdns-router/tests/integration  26.034s
```

含 `TestRoutingSplitSendsEachNameToOnlyItsOwnBranch`、`TestAChinaAnswerIsByteIdenticalWithTheRewritePluginInstalled`、`TestTheReadinessProbeNameIsNotInTheChinaList`、`TestAChinaListWithNoRulesSendsEveryNameAbroad`。测量后已还原文件，`git status` 干净。

### 8.3 一次应当记下的测量陷阱

本次统计唯一域名数时，`sort -u` 在 `LANG=zh_CN.UTF-8` 下给出 111,147，字节精确（`LC_ALL=C`）给出 111,287，相差 140。locale 相关的排序规则合并了不同的字节序列。

**任何对上游列表的计数与去重都必须字节精确**，因为 Go 实现用的是 map + 字符串精确比较。shell 统计必须显式 `LC_ALL=C`。

## 9. 明确不做的事

**`bogus-nxdomain.china.conf` 不采用。** 它列的不是域名而是 165 个裸 IP（`123.125.81.12`、`101.226.10.8` 等），是 DNS 污染源地址。`bogus-nxdomain=<IP>` 是 dnsmasq 用来"丢弃这些上游应答"的抗污染指令。本项目不跑 dnsmasq，境外流量走 dnscrypt-proxy（自带证书校验抗污染），没有任何消费这个列表的位置。用它需要新做一套"检测并丢弃受污染应答"的能力，那是独立功能而非换源。

**不引入任何 DNS 应答改写。** 见 §2.2。

**不改 `configs/mosdns.yaml` 的路由图。** 见 §6。

**不新增运行时回退开关。** "实在不行才回退"是发布期决策；给操作员一个手动切回 v2fly 的开关属于未被要求的功能（YAGNI）。若将来需要，是一次独立请求。

## 10. 已知代价

1. **过度捕获**：111,287 个域名改走国内，其中必然包含使用中国 CDN 的境外站点。这是该列表的设计意图，但属于真实行为变更。
2. **包体积**：发布列表 168 KB → 约 2.2 MB，deb 约 16.2 MB → 约 18.4 MB。
3. **lock 断裂**：v1 lock 需重新 pin。旧机器升级后第一次 `update-lists --check` 会看到一个明确的迁移提示，而不是沉默的旧列表。
4. **两个源的更新节奏不同**：主源每日多次更新，补充源随 v2fly 更新。二者共享一个 `list_sha256`，任一变化都会改变它。

## 11. 改动面

| 文件 | 改动 |
|---|---|
| `internal/rules/chinaconf.go` | 新增：dnsmasq conf 解析与转换 |
| `internal/rules/convert.go` | 保留 v2fly 全部逻辑；增加按 kinds 取规则的能力，供补充使用 |
| `internal/rules/download.go` | `SourceLock` 升 v2；支持多 entry |
| `internal/rules/publish.go` | 支持发布两组 pair（主源 + 回退） |
| `cmd/mosdns-cdnctl/update_lists.go` | 按 `repository` 分派转换器；发布期两分支决策 |
| `packaging/debian/postinst` | 随包发布四个文件 |
| `configs/source-lock.json` | 重写为 v2 |
| `configs/cn-domains.txt` | 重新生成为主源并集 |
| `internal/mosdnsconfig/render.go` | **不改** |

## 12. 测试策略

- **转换器单元测试**：合法行、`#` 注释截断、空行、**转换结果中不出现任何地址**、6 条停用记录降级为注释且注释里没有地址、形状不认识即失败、0 条即失败、重复域名折叠。
- **地址边界门禁**：用 `felixonmars/dnsmasq-china-list` 的真实三个 entry 作为夹具跑转换，把产物交给既有的 `FORBIDDEN_ADDRESSES` 扫描。这一条是"上游无权指定解析器"的执行形式——不是靠 spec 承诺，是靠现有的地址黑名单扫出来。
- **lock schema 测试**：v2 往返编解码、未知字段拒绝、尾随数据拒绝、v1 拒绝并指向重新 pin 的命令、`supplement` 省略的合法形状。
- **护栏测试**：主源条数塌缩被拒；补充条数变化被拒；跨源的 92.5% 跌幅不能由刷新路径发布。
- **已提交清单测试**：`configs/cn-domains.txt` 与 `configs/source-lock.json` 的 `list_sha256` 一致（现有 `internal/rules/committed_test.go` 扩展）；发布列表不含任何地址（现有包内容门禁）。
- **端到端**：集成套件用新列表跑通；Podman `routing` 场景三版本通过。
- **TDD 强制**：每个任务先写失败测试并观察其失败，再写实现。
