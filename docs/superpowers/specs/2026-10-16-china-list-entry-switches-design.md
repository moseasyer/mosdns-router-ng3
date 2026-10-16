# China 列表 entry 开关 — 设计

## 1. 背景

主源 `felixonmars/dnsmasq-china-list` 里被转换的不是一个文件，是三个：

| entry | `server=` 规则数 | 占已发布列表 |
|---|---|---|
| `accelerated-domains.china.conf` | 110,205 | **99.75%** |
| `apple.china.conf` | 165 | 0.15% |
| `google.china.conf` | 112 | 0.10% |
| 去重后合计 | **110,481** | |

三个都是固定转换的，操作员无法只取其中一部分。本次变更给操作员这个能力，但**只给 `google` 与 `apple`**：`accelerated` 不是一类可选项，它是整张列表的主体，把它做成开关等于给出一键关闭几乎全部国内分流的按钮，那不是合理的用户意图。

本变更之前，`accelerated` / `google` / `apple` 三者地位完全相同，都由 `chinaconf.go` 里的一个硬编码切片 `ChinaConfEntries` 决定。

## 2. 中心原则：开关是内容，更新是时间

这是本设计的全部出发点，也是它与"把开关当成一个需要仲裁的策略"这种理解的区别：

| | 管什么 | 字段 |
|---|---|---|
| 更新节奏 | 多久去拉一次上游 | `lists.china.automatic`、`schedule` |
| entry 开关 | 拉回来的东西里**转换哪些** | `lists.china.google`、`lists.china.apple` |

两者**正交**。开启自动更新时，无论 `google` 是开是关，每天都去拉最新上游；拉下来之后按开关决定转换哪几个 entry。关闭 `google` 不意味着"不再更新"，只意味着"更新回来的列表里没有 google"。

因此开关不是一个需要被仲裁的策略，而是一个**转换参数**：凡是生成列表的路径都按它转换，不存在谁覆盖谁的问题。

## 3. 配置形态

`/etc/mosdns/policy.yaml`（仓库内 `configs/policy.yaml`）：

```yaml
lists:
  china:
    automatic: false     # 既有：管更新节奏
    google: true         # 新增：管内容里算不算 google
    apple: true          # 新增：管内容里算不算 apple
```

### 3.1 字段类型必须是 `*bool`

Go 的 `bool` 零值是 `false`，而本设计的默认值是**开**。若用 `bool`，"未写这个字段"会被解析成"关闭"，与默认值相反且无法察觉。

故两个字段都是 `*bool`：省略（`nil`）= 开启，显式 `false` = 关闭。

`configs/policy.yaml` 中两个字段都显式写出，默认值不靠省略表达——因为那个文件是生成产物（见 §3.3），省略会让"默认值"变成一个看不见的隐式约定。

### 3.2 schema_version 保持 1

**不升版本。** `internal/config/policy.go:131` 是硬检查 `p.SchemaVersion != 1`，升到 2 会让每一台已部署机器上现有的 policy 被直接拒绝，那是一次破坏性升级，而本变更并不需要它：新增的两个字段是可选的，旧 policy 不含它们，`nil` 的含义（开启）恰好就是既有行为。

代价是方向性的：新 policy 里的新字段会被**旧二进制**当作未知键拒绝。这是可以接受的——降级本来就不受支持，且拒绝是响亮的，不是静默的。

### 3.3 仓库内的 `configs/policy.yaml` 是生成产物

该文件是 `config.Marshal(config.Defaults())` 的输出，`internal/config/marshal_test.go` 逐字节比对。所以它**不得手工编辑**——改 `Defaults()`，再重新生成。文件头已有注释说明这一点，本次不重复。

### 3.4 操作员的编辑能跨升级保留

`/etc/mosdns/policy.yaml` 已在 `packaging/debian/conffiles` 中，升级时 dpkg 会询问而不是静默覆盖。因此 `google: false` 不会在下一次升级时变回 `true`。本次变更不需要改 conffiles。

## 4. entry 集合的构造

```
固定包含：accelerated-domains.china.conf
可选：    google.china.conf   ← policy.lists.china.google
          apple.china.conf    ← policy.lists.china.apple
```

`chinaconf.go` 现有的 `ChinaConfEntries []string` 拆成两部分：一个固定项常量，和一张"policy 字段名 ↔ 文件名"的可选表。

保留现有属性：**上游新增文件不会被自动纳入**。新增一个可选 entry 必须同时改 policy schema 与这张表——这是有意的摩擦，不是待修的重复。

`accelerated` 缺失即整个转换失败，这个既有属性不变。

## 5. 两条路径都遵守

`update-lists --automatic`（定时器）与 `update-lists --pin-remote`（手动）都读 policy、都按开关转换。

`--pin-remote` 此前完全不读 policy；本变更之后要读。这是本设计唯一的行为代价，第 9 节记录了它。

## 6. 锁的语义

`primary.entries` 记录**这次实际包含的** entry 名。因此一份关掉 google 的锁，`entries` 是 2 个，`list_sha256` 对应那份 2 个 entry 的字节。锁自描述，不需要新增字段。

校验规则从"必须恰好 3 个、且第 4 个按名拒绝"改为：

1. 必须包含 `accelerated-domains.china.conf`；
2. 其余只能是已知可选 entry 的子集；
3. 未知 entry 名仍然拒绝（按名）。

`internal/rules/committed_test.go` 中断言 `len(lock.Primary.Entries) != 3` 的那条改为断言"包含 `accelerated`"，而不是"恰好 3 个"——后者会把默认全开的快照锁死，也会在操作员关掉一个开关之后误报。

## 7. `--check` 报告 entry 集合的漂移

`--check` 现在判定的是主源 commit 与补充源 commit 是否漂移。新增第三项：policy 期望的 entry 集合与已发布锁的 `entries` 是否一致。

不一致时报告，并进入 `up-to-date: false`。理由与已有的两条判定相同——一个报告若只覆盖它所拥有的来源的一半，那半个来源上的变化就无人报告。

## 8. 护栏

**阈值不变，不加豁免。**

关掉 `google` + `apple` 最多掉 277 条（112 + 165），占 0.251%；`0.95` 下限是 104,957，关闭后仍有 110,204 条，余量 5,247 条。护栏天然放行。

但报告里要说清楚：本次发布若 entry 集合与上次不同，明确写出"本次比较跨越了 entry 集合变更"。一个操作员主动的配置变更与一个上游塌缩，结果都是"条数变少"，含义完全不同。不需要例外规则，只需要报告说清楚。

## 9. 失败与拒绝

| 情况 | 行为 |
|---|---|
| policy 读不出来 | **拒绝**（`exitInvalidCLI`），不猜默认值 |
| policy 里有未知键 | 拒绝（本项目一贯要求） |
| entry 名未知 | 拒绝 |

`--automatic` 现在已经是这个行为（`update_lists.go:807`）。`--pin-remote` 改为一致。

**为什么读不出来要拒绝而不是用默认值：** 猜错的后果是"静静地少装了一类规则"，操作员看不出任何异常，只会觉得某些国内域名走了国外。这是最难发现的错误类型。

**已知的代价：** 一个 policy 损坏的机器将无法手动 `--pin-remote`。接受这个代价，因为 policy 损坏意味着 `automatic` 与 `schedule` 同样不可信，此时"手动 pin 能成功"给的是虚假的安全感。

## 10. 兼容性

**已发布快照完全不受影响。** 默认全开，因此：

- `configs/cn-domains.txt` 仍是当前那 110,551 条；
- `configs/source-lock.json` 的 `entries` 仍是 3 个；
- 不需要重新 pin；
- 不需要重新构建包或重新发布。

`accelerated` 固定开启这一点，正是这个结论成立的原因。

## 11. 测试

| 关注点 | 断言 |
|---|---|
| policy 解析 | 省略字段 = 开启；`false` = 关闭；未知键失败 |
| 转换 | 关掉 `google` 后已发布列表不含 google 域名 |
| 锁 | 关掉 `google` 的锁 `entries` 为 2 且包含 `accelerated`；`list_sha256` 与字节相符 |
| 校验 | 缺 `accelerated` 拒绝；未知 entry 名拒绝 |
| 两条路径 | 刷新与 `--pin-remote` 各自遵守开关 |
| 拒绝 | policy 不可读时 `--pin-remote` 退出码为 `exitInvalidCLI` |
| `--check` | policy 期望集合与已发布 `entries` 不一致时报漂移 |
| 回归 | 默认全开时，仓库内 `configs/policy.yaml` 与 `Marshal(Defaults())` 逐字节相同 |

## 12. 不做的事

- 不加命令行 flag 覆盖（会让"policy 与命令行谁说了算"重新变成一个需要仲裁的问题）
- 不做 Web / LuCI 界面
- 不给 `accelerated` 做开关
- 不动 `automatic` 与 `schedule` 的既有语义
- 不让上游新增文件自动纳入 entry 集合

## 13. 与本变更相邻的已知问题（不在本设计范围）

`domain_set` 插件在**列表文件存在但为空**时不会失败，而是构造一个空匹配器，于是所有域名走国外（fail open）；文件不存在时才拒绝启动（fail closed）。上游 `mosdns v5.3.4`
`plugin/data_provider/domain_set/domain_set.go` 的 `NewDomainSet` 里 `if m.Len() > 0` 决定了这一点。

`update-lists` 的防塌缩护栏使得正常路径造不出空列表，因此这不是本变更引入的。但它是一个真实存在的失败形态，且与本变更相邻（同样是"列表内容的有效性"）。单独立项处理。

调研显示业界对此也没有专门处理，但 mihomo 的 `component/geodata/init.go` 里 `Verify(LoadGeoSiteMatcher("CN"))`
是一个可借鉴的形态：**不要用"文件在"作为"数据可用"的判据，要用"解析成功且关键集合非空"**。