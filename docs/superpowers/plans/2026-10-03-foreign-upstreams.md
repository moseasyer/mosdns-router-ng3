# 境外上游多路可配 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把焊死在 Go 常量里的境外上游变成策略里可配的列表，默认 DNSCrypt Quad9 + DoQ Quad9，并加上连通性检测、缓存时间可配与清空。

**Architecture:** `internal/config` 承载新字段与全部校验规则（包括「必须有一个能当 ECH 来源的启用条目」这条闸门）；`internal/mosdnsconfig` 把条目渲染成 `forward.upstreams` 列表并推导 `cdn_rewrite.foreign_upstream`；`cmd/mosdns-cdnctl` 加两个新 verb。DNSCrypt stamp 值不动，仍在 conffile。

**Tech Stack:** Go 1.25.8、mosdns v5.3.4（`github.com/IrineSistiana/mosdns/v5`）、dnscrypt-proxy 2.1.18、yaml.v3、`miekg/dns`。

**Spec:** `docs/superpowers/specs/2026-10-03-foreign-upstreams-design.md`

## 已核实的外部引用

下面每一条都是在这台机器上对着 `mosdns v5.3.4` 的源码核过的，**行号是量出来的**。计划正文引用它们时用的是同一组行号；实现时若发现对不上，以这里的为准，并顺手把正文改掉——一处错的行号引用会让后来的人去读一段无关的代码，然后以为自己懂了。

| 事实 | 位置 |
|---|---|
| `upstream.NewUpstream(addr string, opt Opt)`；scheme 分派 | `pkg/upstream/upstream.go:131`、`:274-553` |
| `Opt.Logger *zap.Logger`、`Opt.Bootstrap string`（**字符串**，逗号分隔） | `pkg/upstream/upstream.go` 的 `Opt` |
| `Upstream.ExchangeContext(ctx, []byte) (*[]byte, error)` | `pkg/upstream/upstream.go:64` |
| `pool.GetBuf` / `pool.ReleaseBuf` 是**变量别名**，不是函数（`= _pool.Get` / `_pool.Release`） | `pkg/pool/allocator.go:28-29` |
| `concurrent` 的选择：`rand.IntN` + `us[(r+i)%len(us)]` | `plugin/executable/forward/forward.go:265-267` |
| 每次尝试一个独立的 5 秒上下文 | `forward.go:272` |
| 收集循环在第一个非错误答案就返回 | `forward.go:302-319` |
| `cache` 把 `LazyCacheTTL > 0` 当**布尔**用（stale-while-revalidate） | `plugin/executable/cache/cache.go:198` |
| cache 无条件 `RegAPI`（→ `/plugins/<tag>/flush`） | `cache.go:108`（handler 在 `:320-324`） |
| 插件 mux 挂在 `/plugins/<tag>` | `coremain/mosdns.go:164-165`（插件侧调用在 `coremain/plugin.go:199-201`） |
| `api.http` 为空则不起 HTTP 服务 | `coremain/mosdns.go:67` |
| `ApplyMaximumTTL(m, ttl)` → `applyTTL(m, ttl, true)` | `pkg/dnsutils/msg.go:65-67` → `:94-112`，夹紧在 `:101-104` |
| `ttl` 插件的守卫：`t.min > 0` / `t.max > 0` | `plugin/executable/ttl/ttl.go:88` / `:91` |
| `cdn_rewrite` 的 ECH 上游必须 `tcp://` | `plugin/executable/cdn_rewrite/cdn_rewrite.go:353` |

---

## Global Constraints

这些是本计划全程适用的项目级要求，逐字取自 spec 或代码。每个 task 的要求都隐含包含本节。

- **ECH 上游必须是 `tcp://`。** `plugin/executable/cdn_rewrite/cdn_rewrite.go:353` `refuseUnsafeUpstream` 一字不改。DoH/DoT/DoQ 可以进 forward 列表，不能当 ECH 来源。
- **`dnscrypt.Render` 的签名不变**，listen 地址仍是 `127.0.0.1:15353`（`internal/dnscrypt/config.go:27`）。
- **`dnscrypt-proxy.toml` 的 stamp 集合不由策略控制。** `dnscrypt.Render(policy, stamps)` 的 `stamps` 参数保持现状。
- **DNSCrypt stamp 值留在 conffile**，手改逃生口保留。
- **禁用 `dnscrypt` 条目不停止 `dnscrypt-proxy` unit。** 安装器继续要求它在 `127.0.0.1:15353` 应答（`installer/mosdns_installer.py:3811-3824`）。
- **无 scheme 的裸地址一律拒绝**——它会默默变成 `udp://`。
- **`ttl_max: 0`（默认）时整个 `foreign_ttl` 插件不渲染**，因为 `ApplyMaximumTTL(m, 0)` 会把每条记录设成 TTL 0（`pkg/dnsutils/msg.go:65-67` 转 `:94-112`，夹紧在 `:101-104`）。
- **策略不变 ⇒ `configs/mosdns.yaml` 逐字节不变。** `internal/mosdnsconfig/render_test.go:905` 是这条性质的唯一守卫。
- **`api.http` 只绑 loopback**（`127.0.0.1:15354`），与本包「只有 loopback listener」的承诺一致。
- **不新增 systemd unit 或 timer。**
- **本仓纪律（反复吃过亏的那几条）：** 期望改变的断言要**改写成守住新性质**，不是删掉；测试的期望值从**被测的源**推导，不 import 被测的常量；一个测试必须能抓住自己的缺陷——**每个新测试都要做一次变异验证**（改坏实现，确认它变红），把结果写进 commit message。
- **环境：**
  ```bash
  export PATH=/tmp/opencode/go1.25.8/bin:$PATH
  export GOCACHE=/home/ubuntu/project_v2/.worktrees/mosdns-router/.gocache-review
  export GOTMPDIR=/home/ubuntu/.gotmp-mosdns
  export TMPDIR=/home/ubuntu/.gotmp-mosdns
  ```
  `cd /home/ubuntu/project_v2/.worktrees/mosdns-router`。**不要 `git stash -u`**（`.gocache-review` 是 753MB，恢复会失败）。

---

## Review Focus

spec 暗示但没有 task 的测试覆盖的输入/失效模式，按「最可能咬到人」排序。每一条在下面 owning task 的步骤里都补了对应的测试。

1. **操作员加了一条 `udp://` 上游。** 上游通、国外域名能解析，但 UDP 传输正是 ECH 那条实测拒绝的，而 `udp://` 在 forward 列表里是**允许**的。合理预期：这条配置要么被拒绝，要么至少不成为 ECH 来源。本计划：Task 1 校验只对**无 scheme** 拒绝，显式 `udp://` 允许进 forward 列表（spec §3.4(4) 把 `udp` 列入允许集合），但 ECH 来源推导（Task 2）只在 `dnscrypt` 或 `tcp://` 里选——**测试必须证明 `udp://` 条目存在时 ECH 来源仍然不是它**。Task 2 Step 4。
2. **操作员把 `dnscrypt` 条目 `enabled: false`，且没有 `tcp://` 条目。** 合理预期：渲染拒绝并点名 ECH，而不是渲染出一个 ECH 会 fail closed 的文档。Task 1 Step 5 + Task 2 Step 5。
3. **操作员把 `concurrent` 设成 1，然后抱怨「多个上游没有 redundancy」。** 合理预期：`1` 是合法配置（mosdns 自己的默认值），但文档说清它没有故障转移。Task 7 Step 4 的 man page 断言。
4. **上游域名解析不了（`bootstrap` 写错或为空）。** 合理预期：`check-upstream --all` 报出**这一条**不可达并点名，而不是整体失败或整体成功。Task 3 Step 4。
5. **两条上游一条长期死掉。** 合理预期：查询仍成功（第一个非错误答案就返回），但日志会有每次失败一行 `Warn`。Task 6 Step 5 的 Podman 断言 + Task 7 文档。

---

## File Structure

| 文件 | 责任 | 本计划中的动作 |
|---|---|---|
| `internal/config/policy.go` | 策略类型与全部校验 | 加 `ForeignUpstream` / `UpstreamKind` / `ForeignCache` 类型；加 `Validate()` 的八条规则；加 scheme 允许集合 |
| `internal/config/foreign_upstream.go` | **新建**：上游条目与 scheme 的全部知识 | 类型定义、`ValidUpstreamScheme`、`ECHSource` 推导、`ParseUpstreamAddr`。单独一个文件是因为 `policy.go` 已 210 行，而这些是独立的一块知识 |
| `internal/config/defaults.go` | 默认策略与渲染出的 `configs/policy.yaml` | 加两条默认上游、`concurrent: 2`、`foreign_cache` 块 |
| `internal/config/load_test.go` | 策略的字面量副本 + 加载 | 更新两处字面量；加 zone 分散门禁 |
| `internal/config/foreign_upstream_test.go` | **新建**：条目校验与 ECH 推导的测试 | 本计划新增的主要测试面 |
| `internal/dnscrypt/config.go` | 渲染 dnscrypt-proxy.toml | 导出 `ListenAddress()`，让 `mosdnsconfig` 有一个单一真相源 |
| `internal/mosdnsconfig/render.go` | 渲染 mosdns.yaml | forward 列表、`concurrent`、`api` 块、ECH 来源推导、可选 `foreign_ttl` |
| `internal/mosdnsconfig/render_test.go` | 渲染的测试 | 改写 §5.1 的九处断言；新增四条 |
| `cmd/mosdns-cdnctl/check_upstream.go` | **新建**：`check-upstream` verb | 整个 verb |
| `cmd/mosdns-cdnctl/flush_cache.go` | **新建**：`flush-cache` verb | 整个 verb |
| `cmd/mosdns-cdnctl/update_lists.go` | `services` 结构与生产实现 | 加 `newUpstream` 注入点（`check-upstream` 的边界） |
| `cmd/mosdns-cdnctl/main.go` | verb dispatch 表 | 加两个 `case` |
| `installer/tests/test_units.py` | 对已发布文档的断言 | 改写 `test_the_routing_document_forwards_to_the_loopback_resolver` |
| `tests/podman/scenarios/routing_test.py` | Podman 路由场景 | 新增「一条上游不可达时查询仍成功」断言 |
| `packaging/man/mosdns-cdnctl.1` | 操作员文档 | 新增上游章节、两个新 verb、`concurrent` 的含义 |
| `configs/policy.yaml` | 生成的默认策略 | 由 `MOSDNS_ROUTER_UPDATE=1` 重新生成，不手改 |

---

### Task 1: 上游条目的类型与校验

**Files:**
- Create: `internal/config/foreign_upstream.go`
- Create: `internal/config/foreign_upstream_test.go`
- Modify: `internal/config/policy.go:24-37`（加类型）、`internal/config/policy.go:85-152`（加校验）
- Modify: `internal/config/defaults.go`（加默认条目）
- Modify: `internal/config/load_test.go:34-45`、`:70-78`（更新两处字面量）

**Interfaces:**
- Consumes: 无（本计划的第一块）
- Produces:
  ```go
  // internal/config/foreign_upstream.go
  type UpstreamKind string
  const (
      UpstreamKindDNSCrypt UpstreamKind = "dnscrypt"
      UpstreamKindUpstream UpstreamKind = "upstream"
  )

  type ForeignUpstream struct {
      Kind      UpstreamKind `yaml:"kind"`
      Name      string       `yaml:"name"`
      Enabled   *bool        `yaml:"enabled"`
      Addr      string       `yaml:"addr"`
      Bootstrap []string     `yaml:"bootstrap"`
  }

  // IsEnabled reports whether the entry routes queries. A nil Enabled is true, so
  // the zero value of a decoded entry is a usable entry.
  func (u ForeignUpstream) IsEnabled() bool

  // ValidUpstreamScheme reports whether mosdns v5.3.4 accepts the scheme.
  // Measured at pkg/upstream/upstream.go:274-553 and :131-156.
  func ValidUpstreamScheme(scheme string) bool

  // ValidateOneUpstream is the whole of spec §3.4 rules 2-6 for one entry.
  func ValidateOneUpstream(u ForeignUpstream) error

  // ValidateForeignUpstreams is rules 1, 2, 3 and 7 for the whole list.
  func ValidateForeignUpstreams(upstreams []ForeignUpstream) error

  // ECHSource returns the upstream the ECH key may be fetched through, or "" when
  // no enabled entry can serve one. Rule 7 made that unreachable for a validated
  // policy, so "" means "this list was never validated".
  func ECHSource(upstreams []ForeignUpstream, dnscryptListener string) string

  // ErrNoECHSource is what rule 7 returns. It is a sentinel so the renderer can
  // tell this refusal from a malformed address.
  var ErrNoECHSource = errors.New(...)
  ```

  ```go
  // internal/config/policy.go additions
  type ForeignPolicy struct {
      DefaultProvider string           `yaml:"default_provider"`
      ECS             bool             `yaml:"ecs"`
      Concurrent      int              `yaml:"concurrent"`
      Upstreams       []ForeignUpstream `yaml:"upstreams"`
  }

  type ForeignCache struct {
      Size    int `yaml:"size"`
      TTLMax  int `yaml:"ttl_max"`
      TTLMin  int `yaml:"ttl_min"`
  }

  type Policy struct {
      ...
      ForeignCache ForeignCache `yaml:"foreign_cache"`
      ...
  }
  ```

- [ ] **Step 1: 写失败的测试——scheme 允许集合**

`internal/config/foreign_upstream_test.go`：

```go
package config

import (
	"strings"
	"testing"
)

// The measured surface of pkg/upstream/upstream.go:274-553 plus the normalisation
// at :131-156. A scheme outside this set is refused by mosdns at construction,
// and the refusal would happen inside the router rather than at render time.
func TestOnlyTheSchemesMosdnsActuallyAcceptsAreValid(t *testing.T) {
	for _, scheme := range []string{"udp", "tcp", "tcp+pipeline", "tls", "tls+pipeline", "https", "h3", "quic", "doq"} {
		if !ValidUpstreamScheme(scheme) {
			t.Errorf("mosdns v5.3.4 accepts %q and ValidUpstreamScheme refuses it", scheme)
		}
	}
	for _, scheme := range []string{"", "sdns", "http", "dot", "ssh", "h2", "TLS", "TCP"} {
		if ValidUpstreamScheme(scheme) {
			t.Errorf("ValidUpstreamScheme accepts %q, which mosdns v5.3.4 does not "+
				"(and an EMPTY scheme is the one that silently becomes udp://)", scheme)
		}
	}
	// And the empty scheme in its own case, because that is the one an operator
	// types by accident and the one the refusal message has to name.
	if err := ValidateOneUpstream(ForeignUpstream{
		Kind: UpstreamKindUpstream, Name: "bare", Addr: "1.1.1.1:53",
	}); err == nil || !strings.Contains(err.Error(), "no transport") {
		t.Fatalf("a bare address was accepted or refused for the wrong reason; got %v", err)
	}
}
```

- [ ] **Step 2: 运行，确认它红**

```
go test ./internal/config/ -run TestOnlyTheSchemesMosdnsActuallyAcceptsAreValid -v
```
Expected: 编译失败，`undefined: ValidUpstreamScheme`。

- [ ] **Step 3: 写最小实现——`foreign_upstream.go` 的 scheme 部分**

```go
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// UpstreamKind is what a foreign upstream entry routes through.
type UpstreamKind string

const (
	// UpstreamKindDNSCrypt is this package's resolver process, reached on its
	// loopback listener. The entry carries no address: the listener is
	// dnscrypt.ListenAddress(), so the two generated documents cannot disagree.
	UpstreamKindDNSCrypt UpstreamKind = "dnscrypt"
	// UpstreamKindUpstream is an address mosdns dials itself.
	UpstreamKindUpstream UpstreamKind = "upstream"
)

// ForeignUpstream is one entry of the foreign branch's upstream list.
type ForeignUpstream struct {
	Kind UpstreamKind `yaml:"kind"`
	// Name is unique and is what a report, a refusal and the control centre name
	// this entry by. Without it an operator is reading a URL.
	Name string `yaml:"name"`
	// Enabled is a pointer so the zero value of a decoded entry is enabled and the
	// rendered policy says `enabled: false` rather than leaving it out.
	Enabled *bool `yaml:"enabled"`
	// Addr is the URL mosdns dials. Required for `upstream`, refused for `dnscrypt`.
	Addr string `yaml:"addr"`
	// Bootstrap resolves THIS entry's own host name and is never a query path.
	// The machine's only resolver is the router, so a domain upstream without one
	// cannot be dialled at all.
	Bootstrap []string `yaml:"bootstrap"`
}

// IsEnabled reports whether the entry routes queries.
func (u ForeignUpstream) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// validUpstreamSchemes is every scheme mosdns v5.3.4 accepts, MEASURED at
// pkg/upstream/upstream.go:274-553 with the normalisation at :131-156 (`h3`
// becomes https, `+pipeline` becomes its base). `sdns` is absent because DNSCrypt
// is not a transport mosdns can dial -- which is why this package runs
// dnscrypt-proxy as a separate process.
var validUpstreamSchemes = map[string]bool{
	"udp": true, "tcp": true, "tcp+pipeline": true,
	"tls": true, "tls+pipeline": true,
	"https": true, "h3": true,
	"quic": true, "doq": true,
}

// ValidUpstreamScheme reports whether mosdns v5.3.4 accepts the scheme.
func ValidUpstreamScheme(scheme string) bool { return validUpstreamSchemes[scheme] }
```

- [ ] **Step 4: 运行，确认它绿**

```
go test ./internal/config/ -run TestOnlyTheSchemesMosdnsActuallyAcceptsAreValid -v
```
Expected: PASS。

- [ ] **Step 5: 写失败的测试——`ValidateOneUpstream` 的每条规则**

追加到 `internal/config/foreign_upstream_test.go`：

```go
// One table, one row per rule in spec §3.4 rules 2-6, and each row says which
// rule it is so a failure names the rule rather than the symptom.
func TestEachAddressRuleRefusesExactlyWhatItClaims(t *testing.T) {
	ok := ForeignUpstream{Kind: UpstreamKindUpstream, Name: "n", Addr: "quic://dns.quad9.net:853"}
	cases := []struct {
		rule string
		entry ForeignUpstream
		wants string
	}{
		{"not a URL", withAddr(ok, "://"), "not a URL"},
		{"no scheme", withAddr(ok, "dns.quad9.net:853"), "no transport"},
		{"unknown scheme", withAddr(ok, "sdns://dns.quad9.net"), "scheme"},
		{"credentials", withAddr(ok, "https://user:pw@dns.quad9.net/dns-query"), "credentials, path or query"},
		{"path", withAddr(ok, "https://dns.quad9.net/dns-query"), "credentials, path or query"},
		{"query", withAddr(ok, "https://dns.quad9.net/dns-query?a=b"), "credentials, path or query"},
		{"loopback host", withAddr(ok, "tcp://127.0.0.1:5353"), "loopback"},
		{"unspecified host", withAddr(ok, "tcp://0.0.0.0:53"), "loopback"},
		{"link-local host", withAddr(ok, "fe80::1:53"), "loopback"},
		{"port 53", withAddr(ok, "tcp://9.9.9.9:53"), "system resolver"},
		{"bootstrap loopback", withBootstrap(ok, "127.0.0.1:53"), "loopback"},
		{"bootstrap not IP:port", withBootstrap(ok, "9.9.9.9"), "IP address and a port"},
		{"dnscrypt with addr", ForeignUpstream{Kind: UpstreamKindDNSCrypt, Name: "d", Addr: "tcp://127.0.0.1:15353"}, "must not carry an addr"},
		{"upstream without addr", ForeignUpstream{Kind: UpstreamKindUpstream, Name: "u"}, "must carry an addr"},
		{"unknown kind", ForeignUpstream{Kind: "carrier-pigeon", Name: "p"}, "kind"},
		{"empty name", withAddr(ForeignUpstream{Kind: UpstreamKindUpstream, Addr: "quic://a.example:853"}, ""), "name"},
		{"bad name characters", withAddr(ForeignUpstream{Kind: UpstreamKindUpstream, Name: "Quad9 DoQ", Addr: "quic://a.example:853"}, ""), "name"},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			err := ValidateOneUpstream(c.entry)
			if c.wants == "" {
				if err != nil {
					t.Fatalf("rule %q should have accepted %+v: %v", c.rule, c.entry, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("rule %q accepted %+v", c.rule, c.entry)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("rule %q refused %+v for the wrong reason: %v", c.rule, c.entry, err)
			}
		})
	}
}

func TestAWellFormedEntryIsAccepted(t *testing.T) {
	enabled := false
	for _, entry := range []ForeignUpstream{
		{Kind: UpstreamKindDNSCrypt, Name: "quad9-dnscrypt"},
		{Kind: UpstreamKindUpstream, Name: "quad9-doq", Addr: "quic://dns.quad9.net:853",
			Bootstrap: []string{"9.9.9.9:53", "149.112.112.9:53"}},
		{Kind: UpstreamKindUpstream, Name: "off", Addr: "tls://dns.quad9.net:853", Enabled: &enabled},
		{Kind: UpstreamKindUpstream, Name: "by-ip", Addr: "https://9.9.9.9/dns-query"},
		{Kind: UpstreamKindUpstream, Name: "bare-ok", Addr: "tcp://[2606:4700:4700::1111]:853"},
	} {
		if err := ValidateOneUpstream(entry); err != nil {
			t.Errorf("a well-formed entry was refused: %+v: %v", entry, err)
		}
	}
}

func TestADisabledEntryStillHasToBeWellFormed(t *testing.T) {
	// A disabled entry with a malformed address is refused, and that is the safe
	// direction: an operator who disables an entry and later re-enables it by
	// flipping one word should not find a configuration that cannot be rendered.
	disabled := false
	err := ValidateOneUpstream(ForeignUpstream{
		Kind: UpstreamKindUpstream, Name: "broken", Addr: "sdns://x", Enabled: &disabled,
	})
	if err == nil {
		t.Fatal("a disabled entry with an unusable addr was accepted, so re-enabling it " +
			"by one word would produce a policy that cannot be rendered")
	}
}

func withAddr(u ForeignUpstream, addr string) ForeignUpstream { u.Addr = addr; return u }
func withBootstrap(u ForeignUpstream, addr string) ForeignUpstream {
	u.Bootstrap = []string{addr}; return u
}
```

- [ ] **Step 6: 运行，确认它红**

```
go test ./internal/config/ -run 'TestEachAddressRuleRefuses|TestAWellFormedEntry|TestADisabledEntry' -v
```
Expected: 编译失败，`undefined: ValidateOneUpstream`。

- [ ] **Step 7: 写 `ValidateOneUpstream`**

追加到 `internal/config/foreign_upstream.go`：

```go
// systemResolverPort is the port a foreign upstream may not be on. It is the
// fallback this project refuses to build, whatever a scheme says about it, and it
// is the same constant the renderer refuses the DNSCrypt listener on.
const systemResolverPort = 53

// ValidateOneUpstream is spec §3.4 rules 2 through 6 for one entry.
//
// A DISABLED entry is validated exactly like an enabled one, deliberately. An
// operator who turns an entry off and later turns it back on by flipping one word
// should not find a configuration that cannot be rendered, and the alternative --
// validating only what is currently routed -- means the refusal arrives at the
// moment the operator re-enables it, which is the worst moment to arrive.
func ValidateOneUpstream(u ForeignUpstream) error {
	if !validUpstreamName(u.Name) {
		return fmt.Errorf("foreign.upstreams[].name must be non-empty and carry only [a-z0-9-], got %q", u.Name)
	}
	switch u.Kind {
	case UpstreamKindDNSCrypt:
		if u.Addr != "" {
			return fmt.Errorf("foreign.upstreams[] kind dnscrypt must not carry an addr (%q): the listener is the packaged resolver's own address, not a configured one", u.Addr)
		}
		if len(u.Bootstrap) > 0 {
			return fmt.Errorf("foreign.upstreams[] kind dnscrypt must not carry a bootstrap list (%v): it resolves no host name", u.Bootstrap)
		}
		return nil
	case UpstreamKindUpstream:
		if u.Addr == "" {
			return fmt.Errorf("foreign.upstreams[] kind upstream must carry an addr")
		}
	default:
		return fmt.Errorf("foreign.upstreams[].kind must be %q or %q, got %q", UpstreamKindDNSCrypt, UpstreamKindUpstream, u.Kind)
	}
	return validateUpstreamAddr(u)
}

func validateUpstreamAddr(u ForeignUpstream) error {
	parsed, err := url.Parse(u.Addr)
	if err != nil {
		return fmt.Errorf("foreign.upstreams[] addr %q is not a URL: %w", u.Addr, err)
	}
	if parsed.Scheme == "" {
		return fmt.Errorf("foreign.upstreams[] addr %q names no transport, so mosdns would dial it as udp://; "+
			"write the scheme out, and note that udp is the one transport this router refuses for "+
			"anything on the ECH path", u.Addr)
	}
	if !ValidUpstreamScheme(parsed.Scheme) {
		return fmt.Errorf("foreign.upstreams[] addr %q has scheme %q, which mosdns v5.3.4 does not accept; "+
			"the accepted schemes are udp, tcp, tcp+pipeline, tls, tls+pipeline, https, h3, quic and doq",
			u.Addr, parsed.Scheme)
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("foreign.upstreams[] addr %q must be a bare scheme://host[:port] URL, with no credentials, path or query", u.Addr)
	}
	if err := refuseLoopbackHost(parsed.Host); err != nil {
		return fmt.Errorf("foreign.upstreams[] addr %q: %w", u.Addr, err)
	}
	if port := parsed.Port(); port != "" && port == "53" {
		return fmt.Errorf("foreign.upstreams[] addr %q is on port %d, which belongs to the system resolver", u.Addr, systemResolverPort)
	}
	for _, resolver := range u.Bootstrap {
		if _, err := netip.ParseAddrPort(resolver); err != nil {
			return fmt.Errorf("foreign.upstreams[] bootstrap %q must be an IP address and a port: %w", resolver, err)
		}
		address, _ := netip.ParseAddrPort(resolver)
		if !address.Addr().IsGlobalUnicast() || address.Addr().IsLoopback() {
			return fmt.Errorf("foreign.upstreams[] bootstrap %q is a loopback or non-global address; "+
				"the bootstrap resolves this entry's own host name and has to be somewhere the router is not", resolver)
		}
	}
	return nil
}

// refuseLoopbackHost refuses a host that would make this router forward foreign
// queries to itself, or to nothing.
func refuseLoopbackHost(host string) error {
	if host == "" {
		return fmt.Errorf("names no host")
	}
	// A bracketed IPv6 literal, with or without a port.
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return fmt.Errorf("has an unterminated IPv6 literal")
		}
		host = host[1:end]
	} else if colon := strings.LastIndex(host, ":"); colon >= 0 && strings.Count(host, ":") == 1 {
		host = host[:colon]
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.IsLoopback() || address.IsUnspecified() || address.IsLinkLocalUnicast() {
			return fmt.Errorf("names %s, which is a loopback, unspecified or link-local address; "+
				"a foreign upstream has to be somewhere this router is not", address)
		}
	}
	return nil
}

// validUpstreamName keeps a name to what can appear in a log line and a report
// without quoting: [a-z0-9-], non-empty. It is deliberately narrower than
// validHostname -- a name is an identifier, not a name to be resolved.
func validUpstreamName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for index := 0; index < len(name); index++ {
		c := name[index]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 8: 运行，确认它绿**

```
go test ./internal/config/ -run 'TestEachAddressRuleRefuses|TestAWellFormedEntry|TestADisabledEntry|TestOnlyTheSchemes' -v
```
Expected: PASS。

- [ ] **Step 9: 写失败的测试——列表级规则与 ECH 来源推导**

追加到 `internal/config/foreign_upstream_test.go`：

```go
func TestTheListRulesRefuseWhatASingleEntryCannot(t *testing.T) {
	cases := []struct {
		rule  string
		list  []ForeignUpstream
		wants string
	}{
		{"empty list", nil, "at least one"},
		{"all disabled", []ForeignUpstream{disabledEntry(UpstreamKindDNSCrypt, "d", "")}, "at least one enabled"},
		{"two dnscrypt", []ForeignUpstream{
			{Kind: UpstreamKindDNSCrypt, Name: "a"}, {Kind: UpstreamKindDNSCrypt, Name: "b"},
		}, "at most one"},
		{"duplicate name", []ForeignUpstream{
			{Kind: UpstreamKindDNSCrypt, Name: "same"},
			{Kind: UpstreamKindUpstream, Name: "same", Addr: "quic://a.example:853"},
		}, "unique"},
		{"no ECH source", []ForeignUpstream{
			{Kind: UpstreamKindUpstream, Name: "doh", Addr: "https://dns.quad9.net/dns-query"},
		}, "ECH"},
		{"disabled dnscrypt and no tcp", []ForeignUpstream{
			disabledEntry(UpstreamKindDNSCrypt, "d", ""),
			{Kind: UpstreamKindUpstream, Name: "doh", Addr: "https://dns.quad9.net/dns-query"},
		}, "ECH"},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			err := ValidateForeignUpstreams(c.list)
			if err == nil {
				t.Fatalf("rule %q accepted %+v", c.rule, c.list)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("rule %q refused for the wrong reason: %v", c.rule, err)
			}
		})
	}
}

func TestADisabledTCPUpstreamDoesNotBecomeTheECHSource(t *testing.T) {
	// Review Focus #1, in the form that matters: a tcp:// entry that is switched
	// off must not be what ECH is fetched through, because the switch is the
	// operator saying "do not use this".
	list := []ForeignUpstream{
		disabledEntry(UpstreamKindUpstream, "off", "tcp://9.9.9.9:5353"),
		{Kind: UpstreamKindDNSCrypt, Name: "on"},
	}
	if got := ECHSource(list, "tcp://127.0.0.1:15353"); got != "tcp://127.0.0.1:15353" {
		t.Fatalf("the ECH source is %q, want the enabled dnscrypt entry's listener", got)
	}
}

func TestTheECHSourceIsTheListenerOrTheFirstEnabledTCPUpstream(t *testing.T) {
	listener := "tcp://127.0.0.1:15353"
	cases := []struct {
		name string
		list []ForeignUpstream
		want string
	}{
		{"dnscrypt wins while enabled", []ForeignUpstream{
			{Kind: UpstreamKindDNSCrypt, Name: "d"},
			{Kind: UpstreamKindUpstream, Name: "t", Addr: "tcp://9.9.9.9:5353"},
		}, listener},
		{"falls to the first tcp when dnscrypt is off", []ForeignUpstream{
			disabledEntry(UpstreamKindDNSCrypt, "d", ""),
			{Kind: UpstreamKindUpstream, Name: "doh", Addr: "https://dns.quad9.net/dns-query"},
			{Kind: UpstreamKindUpstream, Name: "t1", Addr: "tcp://9.9.9.9:5353"},
			{Kind: UpstreamKindUpstream, Name: "t2", Addr: "tcp://1.1.1.1:5353"},
		}, "tcp://9.9.9.9:5353"},
		{"a udp entry is never the source", []ForeignUpstream{
			{Kind: UpstreamKindUpstream, Name: "u", Addr: "udp://9.9.9.9:53"},
			{Kind: UpstreamKindUpstream, Name: "t", Addr: "tcp://9.9.9.9:5353"},
		}, "tcp://9.9.9.9:5353"},
		{"nothing to choose from", []ForeignUpstream{
			{Kind: UpstreamKindUpstream, Name: "u", Addr: "udp://9.9.9.9:53"},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ECHSource(c.list, listener); got != c.want {
				t.Fatalf("ECHSource = %q, want %q", got, c.want)
			}
		})
	}
}

func disabledEntry(kind UpstreamKind, name, addr string) ForeignUpstream {
	off := false
	return ForeignUpstream{Kind: kind, Name: name, Addr: addr, Enabled: &off}
}
```

- [ ] **Step 10: 运行，确认它红**

```
go test ./internal/config/ -run 'TestTheListRules|TestADisabledTCPUpstream|TestTheECHSourceIs' -v
```
Expected: 编译失败，`undefined: ValidateForeignUpstreams`。

- [ ] **Step 11: 写列表级校验与 ECH 推导**

追加到 `internal/config/foreign_upstream.go`：

```go
// ErrNoECHSource is rule 7's refusal, and it is a sentinel so the renderer can
// tell "this policy has no way to fetch an ECH key" from "this policy has a
// malformed address". The two need different operator answers.
var ErrNoECHSource = errors.New(
	"foreign.upstreams must leave at least one ENABLED entry the ECH key can be fetched through: " +
		"either a dnscrypt entry, or an upstream whose addr is tcp://. " +
		"cdn_rewrite refuses every other transport for the ECH fetch on a measured ground " +
		"(mosdns's UDP upstream re-sends an unanswered query and can drop an early answer, and an " +
		"ECH fetch has no client watching it), so a policy that leaves no tcp:// route would let " +
		"every forced-ECH name fail closed once the key's grace ended, with nothing in the log saying why")

// ValidateForeignUpstreams is spec §3.4 rules 1, 2, 3 and 7 for the whole list,
// after ValidateOneUpstream has run over every entry.
func ValidateForeignUpstreams(upstreams []ForeignUpstream) error {
	if len(upstreams) == 0 {
		return fmt.Errorf("foreign.upstreams must name at least one upstream")
	}
	enabled := 0
	dnscrypt := 0
	seen := map[string]bool{}
	for _, u := range upstreams {
		if err := ValidateOneUpstream(u); err != nil {
			return err
		}
		if seen[u.Name] {
			return fmt.Errorf("foreign.upstreams[].name must be unique, and %q appears twice: "+
				"a name is what a report and a refusal identify an entry by", u.Name)
		}
		seen[u.Name] = true
		if u.IsEnabled() {
			enabled++
		}
		if u.Kind == UpstreamKindDNSCrypt {
			dnscrypt++
		}
	}
	if enabled == 0 {
		return fmt.Errorf("foreign.upstreams must leave at least one enabled entry; all %d are disabled, "+
			"which leaves the foreign branch with nowhere to send a query", len(upstreams))
	}
	if dnscrypt > 1 {
		return fmt.Errorf("foreign.upstreams may name at most one dnscrypt entry, and %d do: "+
			"there is one packaged resolver and it listens on one address", dnscrypt)
	}
	if ECHSource(upstreams, "") == "" {
		return ErrNoECHSource
	}
	return nil
}

// ECHSource returns the upstream the ECH key may be fetched through, or "" when
// no enabled entry can serve one.
//
// **The dnscrypt entry wins over a tcp:// upstream even when both are enabled**,
// and that order is the design rather than an accident: the packaged resolver is
// already started, already probed by the installer, and already the road an ECH
// failure has to be diagnosable against. A tcp:// upstream is the fallback for a
// machine that turned the dnscrypt entry off.
//
// Rule 7 makes "" unreachable for a validated policy, so "" means "this list was
// never validated" -- which is why the dnscrypt listener is passed as "" by
// ValidateForeignUpstreams and a real URL by the renderer.
func ECHSource(upstreams []ForeignUpstream, dnscryptListener string) string {
	for _, u := range upstreams {
		if u.IsEnabled() && u.Kind == UpstreamKindDNSCrypt && dnscryptListener != "" {
			return dnscryptListener
		}
	}
	for _, u := range upstreams {
		if u.IsEnabled() && u.Kind == UpstreamKindUpstream && strings.HasPrefix(u.Addr, "tcp://") {
			return u.Addr
		}
	}
	return ""
}
```

- [ ] **Step 12: 运行，确认它绿**

```
go test ./internal/config/ -run 'TestTheListRules|TestADisabledTCPUpstream|TestTheECHSourceIs' -v
```
Expected: PASS。

- [ ] **Step 13: 把新字段接进 `Policy` 与 `Validate()`**

`internal/config/policy.go`：把 `ForeignPolicy` 换成 Step 11 之前展示的形状（加 `Concurrent`、`Upstreams`），在 `Policy` 里加 `ForeignCache`，并在 `Validate()` 的 `Foreign.DefaultProvider` 检查之后插入：

```go
	if err := ValidateForeignUpstreams(p.Foreign.Upstreams); err != nil {
		return err
	}
	// 1 is mosdns's own default and it is a RANDOM PICK, not a race
	// (pkg/executable/forward/forward.go:248-267), so an operator who set it to
	// 1 has a valid policy with no failover between their upstreams. 3 is
	// mosdns's own hard cap, so a larger value would be silently truncated.
	if p.Foreign.Concurrent < 1 || p.Foreign.Concurrent > 3 {
		return fmt.Errorf("foreign.concurrent must be between 1 and 3, got %d; 1 is mosdns's own "+
			"default and means a random pick with no failover, and 3 is the cap mosdns enforces",
			p.Foreign.Concurrent)
	}
	if p.ForeignCache.Size <= 0 {
		return fmt.Errorf("foreign_cache.size must be greater than zero")
	}
	if p.ForeignCache.TTLMax < 0 || p.ForeignCache.TTLMin < 0 {
		return fmt.Errorf("foreign_cache.ttl_max and foreign_cache.ttl_min must not be negative")
	}
```

- [ ] **Step 14: 写失败的测试——`concurrent` 与 `foreign_cache` 的校验**

追加到 `internal/config/foreign_upstream_test.go`：

```go
func TestConcurrentIsBoundedByWhatMosdnsItselfEnforces(t *testing.T) {
	for _, value := range []int{0, -1, 4, 100} {
		policy := Defaults()
		policy.Foreign.Concurrent = value
		if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "foreign.concurrent") {
			t.Errorf("foreign.concurrent = %d was accepted or refused for the wrong reason: %v", value, err)
		}
	}
	for _, value := range []int{1, 2, 3} {
		policy := Defaults()
		policy.Foreign.Concurrent = value
		if err := policy.Validate(); err != nil {
			t.Errorf("foreign.concurrent = %d was refused: %v", value, err)
		}
	}
}

func TestTheCacheBlockIsBoundedToo(t *testing.T) {
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.ForeignCache.Size = 0 },
		func(p *Policy) { p.ForeignCache.TTLMax = -1 },
		func(p *Policy) { p.ForeignCache.TTLMin = -1 },
	} {
		policy := Defaults()
		mutate(&policy)
		if err := policy.Validate(); err == nil {
			t.Errorf("a cache block with %+v was accepted", policy.ForeignCache)
		}
	}
}
```

- [ ] **Step 15: 运行，确认它绿**（Step 13 已经写了实现）

```
go test ./internal/config/ -run 'TestConcurrentIsBounded|TestTheCacheBlockIsBounded' -v
```
Expected: PASS。

- [ ] **Step 16: 写失败的测试——默认值与 zone 分散**

`internal/config/load_test.go`：

```go
func TestTheDefaultForeignRouteHasBothATransportAndADoQ(t *testing.T) {
	policy := Defaults()
	kinds := map[UpstreamKind]int{}
	for _, u := range policy.Foreign.Upstreams {
		if !u.IsEnabled() {
			t.Fatalf("a default upstream %q is disabled, so the shipped policy does not route "+
				"through it and shipping it is a lie", u.Name)
		}
		kinds[u.Kind]++
	}
	if kinds[UpstreamKindDNSCrypt] != 1 {
		t.Errorf("the default route has %d dnscrypt entries, want 1", kinds[UpstreamKindDNSCrypt])
	}
	// The whole point of the change: a second, INDEPENDENT transport out. One that
	// is also a dnscrypt entry would be a second address for the same process.
	if kinds[UpstreamKindUpstream] < 1 {
		t.Errorf("the default route names no mosdns upstream, so a DNSCrypt outage is still a "+
			"total loss of the foreign branch")
	}
	if policy.Foreign.Concurrent < 2 {
		t.Errorf("foreign.concurrent = %d, want at least 2: 1 is a random pick with no failover, "+
			"which is the default this change exists to move away from", policy.Foreign.Concurrent)
	}
	// And the DoQ entry must carry a bootstrap, because the machine's only resolver
	// is this router.
	for _, u := range policy.Foreign.Upstreams {
		if u.Kind == UpstreamKindUpstream && strings.Contains(u.Addr, "://dns.") && len(u.Bootstrap) == 0 {
			t.Errorf("the default upstream %q is a domain address with no bootstrap list, so it "+
				"cannot be dialled on a machine whose only resolver is this router", u.Name)
		}
	}
}

// The default ECH source is the packaged listener, and it stays that way even
// though a tcp:// upstream is present.
func TestTheDefaultECHRouteIsThePackagedResolver(t *testing.T) {
	policy := Defaults()
	got := ECHSource(policy.Foreign.Upstreams, "tcp://127.0.0.1:15353")
	if got != "tcp://127.0.0.1:15353" {
		t.Fatalf("the default ECH source is %q, want the packaged resolver's listener", got)
	}
}
```

- [ ] **Step 17: 运行，确认它红**

```
go test ./internal/config/ -run 'TestTheDefaultForeignRoute|TestTheDefaultECHRoute' -v
```
Expected: FAIL，`the default route has 0 dnscrypt entries`。

- [ ] **Step 18: 写默认条目**

`internal/config/defaults.go`，在 `Defaults()` 里：

```go
		Foreign: ForeignPolicy{
			DefaultProvider: "Quad9 Secure DNSCrypt v2",
			ECS:             false,
			// 2, not mosdns's own 1, and the reason is in the spec: at 1 the
			// selection is `rand.IntN` plus a modular walk (pkg/executable/forward/
			// forward.go:265-267), which is load balancing and not redundancy. This
			// package's own dhcp_forward already uses 2. The cost is one extra
			// outbound attempt per query and a warn line per failure -- NOT latency,
			// because each attempt carries its own timeout and the loop returns on the
			// first non-error answer.
			Concurrent: 2,
			// Two transports out, not two addresses for one process: the packaged
			// DNSCrypt resolver, and a DoQ endpoint mosdns dials itself. Quad9 is the
			// same provider and the same trust root on both roads.
			Upstreams: defaultForeignUpstreams(),
		},
		ForeignCache: ForeignCache{
			// The value the renderer already wrote as a constant
			// (internal/mosdnsconfig/render.go:70), which was explicitly NOT a
			// measurement. Moving it here does not make it one.
			Size: 1024,
			// 0 means no clamp, and the ttl plugin is then not rendered at all --
			// because ApplyMaximumTTL(m, 0) sets every record to TTL 0
			// (pkg/dnsutils/msg.go:65-67 -> :94-112, the clamp at :101-104). 0 is also what keeps
			// the committed
			// routing document byte-identical.
			TTLMax: 0,
			TTLMin: 0,
		},
```

并加：

```go
// defaultForeignUpstreams is the shipped foreign route.
//
// The DoQ entry's bootstrap is the same two plain-DNS Quad9 addresses the DNSCrypt
// renderer already uses (internal/dnscrypt/config.go:62), for the same reason: this
// machine's only resolver is the router, so a domain upstream has to name somewhere
// else to resolve its own name. A bootstrap is not a query path -- foreign queries
// never go there -- and both addresses are outside every set this package refuses.
func defaultForeignUpstreams() []ForeignUpstream {
	return []ForeignUpstream{
		{Kind: UpstreamKindDNSCrypt, Name: "quad9-dnscrypt"},
		{
			Kind:  UpstreamKindUpstream,
			Name:  "quad9-doq",
			Addr:  "quic://dns.quad9.net:853",
			Bootstrap: []string{"9.9.9.9:53", "149.112.112.9:53"},
		},
	}
}
```

- [ ] **Step 19: 重新生成 `configs/policy.yaml` 并更新 `load_test.go` 的两处字面量**

```
MOSDNS_ROUTER_UPDATE=1 go test ./internal/config/ -run TestMarshal -v
```

然后把 `load_test.go` 的 `validPolicyYAML` 的 `foreign:` 段和 `TestDefaultsMatchApprovedSpec` 的字面量 `Policy` 都更新成新形状——**两处都要改，`Sources: echDefaultSources` 那种写法不行**：`TestDefaultsMatchApprovedSpec` 的字面量**就是**「Defaults() 仍然等于被批准的东西」这个断言本身，指向常量会让任何人都能随便换掉它。

- [ ] **Step 20: 运行整个包**

```
MOSDNS_ROUTER_UPDATE=1 go test ./internal/config/ -v
```
Expected: 全 PASS。

- [ ] **Step 21: 变异验证（不是可选的）**

逐条改坏实现，确认对应测试变红。**任何一条抓不住，就把那条测试修到能抓住为止**，并在 commit message 里记录结果。

```bash
cp internal/config/foreign_upstream.go /home/ubuntu/.gotmp-mosdns/fu.bak

# 1. ECH 推导忽略 enabled
python3 - <<'PY'
from pathlib import Path
p = Path('internal/config/foreign_upstream.go'); t = p.read_text()
t = t.replace('if u.IsEnabled() && u.Kind == UpstreamKindDNSCrypt', 'if u.Kind == UpstreamKindDNSCrypt')
p.write_text(t)
PY
go test ./internal/config/ -run 'TestADisabledTCPUpstream|TestTheECHSourceIs' 2>&1 | grep -c 'FAIL'   # 期望 >= 1

# 2. 允许空 scheme
python3 - <<'PY'
from pathlib import Path
p = Path('internal/config/foreign_upstream.go'); t = p.read_text()
t = t.replace('\tif parsed.Scheme == "" {', '\tif false {')
p.write_text(t)
PY
go test ./internal/config/ -run TestOnlyTheSchemes 2>&1 | grep -c 'FAIL'   # 期望 >= 1

# 3. 去掉 ErrNoECHSource 那条闸门
python3 - <<'PY'
from pathlib import Path
p = Path('internal/config/foreign_upstream.go'); t = p.read_text()
t = t.replace('\tif ECHSource(upstreams, "") == "" {\n\t\treturn ErrNoECHSource\n\t}\n', '')
p.write_text(t)
PY
go test ./internal/config/ -run TestTheListRules 2>&1 | grep -c 'FAIL'   # 期望 >= 1

cp /home/ubuntu/.gotmp-mosdns/fu.bak internal/config/foreign_upstream.go
go test ./internal/config/ 2>&1 | tail -2   # 期望 ok
```

- [ ] **Step 22: 提交**

```bash
git add internal/config/
git commit -m "feat(config): the foreign route is a list, with a gate on the ECH source

The foreign branch's upstream was one address in a Go constant
(internal/mosdnsconfig/render.go:186), so 'an operator may substitute a provider'
had been half-implemented for the life of the package: the provider was a comment
in the generated TOML and a policy string nothing read.

foreign.upstreams is now a list whose entries are either the packaged dnscrypt
resolver or an address mosdns dials in any scheme mosdns v5.3.4 accepts --
MEASURED at pkg/upstream/upstream.go:274-553, with no sdns:// among them, which
is why the dnscrypt process exists at all. foreign.concurrent defaults to 2 rather
than mosdns's 1 because at 1 the selection is rand.IntN plus a modular walk
(forward.go:265-267): load balancing, not redundancy.

The one rule that earns the file its keep is the last one. cdn_rewrite refuses
every non-tcp transport for the ECH fetch (cdn_rewrite.go:353) on a measured
ground, so a policy with only DoH upstreams would let every forced-ECH name fail
closed the moment the key's grace ended -- with nothing in the log saying why.
ValidateForeignUpstreams refuses that list, names ECH, and says what to add. The
dnscrypt entry wins over a tcp:// upstream when both are on, because the packaged
resolver is already started and already probed by the installer, so an ECH failure
is diagnosable against it.

A bare address with no scheme is refused with its own message rather than folded
into the unknown-scheme one: mosdns turns it into udp://, and udp is precisely the
transport this router refuses on the ECH path.

A DISABLED entry is still validated, so turning one back on cannot produce a
policy that cannot be rendered.

Mutation-verified: ignoring 'enabled' in ECHSource, allowing an empty scheme, and
dropping the no-ECH-source gate each turn the matching case red."
```

---

### Task 2: 渲染

**Files:**
- Modify: `internal/dnscrypt/config.go:26-28`（导出 `ListenAddress()`）
- Modify: `internal/mosdnsconfig/render.go:44-53`（tag 常量）、`:126-171`（`Paths`）、`:176-190`（`ProductionPaths`）、`:198-201`（`document`）、`:221-265`（args 类型）、`:280-300`（`Render`）、`:307-451`（`renderable`）、`:528-642`（`resolve`）
- Modify: `internal/mosdnsconfig/render_test.go`（§5.1 的九处 + 四条新增）

**Interfaces:**
- Consumes: `config.ForeignUpstream`、`config.ECHSource`、`config.IsEnabled`（Task 1）
- Produces:
  ```go
  // internal/dnscrypt/config.go
  // ListenAddress is the address the rendered resolver document binds, as
  // host:port. It is exported so the routing renderer reads the SAME value the
  // resolver document writes, rather than a second copy of it: two copies are how
  // a test that asserts the two documents agree comes to pass while they do not.
  const ListenAddress = listenAddress  // "127.0.0.1:15353"

  // internal/mosdnsconfig/render.go
  // apiListenAddress is the loopback address the cache plugin's flush endpoint is
  // reached on. Loopback because this package's promise is that every listener it
  // configures is loopback-only, and unprivileged because the router binds 53.
  const apiListenAddress = "127.0.0.1:15354"

  // resolveECHUpstream is the derived ECH source, resolved once and carried on the
  // resolved paths so both documents read the same value.
  ```

- [ ] **Step 1: 写失败的测试——`dnscrypt.ListenAddress` 是单一真相源**

`internal/dnscrypt/config_test.go` 追加：

```go
func TestTheExportedListenerIsTheOneTheDocumentBinds(t *testing.T) {
	rendered, err := Render(Defaults(), Defaults())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(rendered), listenAddressForTest) {
		t.Fatalf("the rendered resolver document does not bind %s", listenAddressForTest)
	}
	if ListenAddress != listenAddressForTest {
		t.Fatalf("ListenAddress is %q but the document binds %q, so the routing renderer "+
			"would read a different address than the resolver listens on", ListenAddress, listenAddressForTest)
	}
}
```

其中 `listenAddressForTest = "127.0.0.1:15353"` 是本文件顶部的常量（已存在，名字是 `foreignListener`，`:27`——直接用它，不要新增）。

- [ ] **Step 2: 运行，确认它红**

```
go test ./internal/dnscrypt/ -run TestTheExportedListener -v
```
Expected: 编译失败，`undefined: ListenAddress`。

- [ ] **Step 3: 导出它**

`internal/dnscrypt/config.go`，把 `const` 块改成：

```go
const (
	// ListenAddress is the address the rendered resolver document binds, as
	// host:port. It is EXPORTED so the routing renderer reads the same value the
	// resolver document writes. A second copy of this string in another package is
	// how a test that asserts the two documents agree comes to pass while they do
	// not agree -- and the agreement is the whole of what the ECH fetch's
	// correctness rests on, because it is fetched through this address.
	ListenAddress = "127.0.0.1:15353"

	listenAddress = ListenAddress

	netprobeHost = "9.9.9.9:443"
)
```

- [ ] **Step 4: 运行，确认它绿**

```
go test ./internal/dnscrypt/ -v
```
Expected: 全 PASS（13 个原有用例 + 1 个新）。

- [ ] **Step 5: 写失败的测试——forward 列表、`concurrent`、ECH 来源、`api` 块**

`internal/mosdnsconfig/render_test.go` 追加：

```go
// Every enabled `upstream` entry becomes exactly one forward upstream, in policy
// order, and a `dnscrypt` entry becomes none -- it is a process, not an address.
func TestEveryEnabledUpstreamEntryBecomesOneForwardUpstream(t *testing.T) {
	policy := config.Defaults()
	policy.Foreign.Upstreams = append(policy.Foreign.Upstreams, config.ForeignUpstream{
		Kind: config.UpstreamKindUpstream, Name: "extra", Addr: "tls://dns.quad9.net:853",
	})
	document, err := Render(policy, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	forward := foreignForward(t, document)
	want := []string{"quic://dns.quad9.net:853", "tls://dns.quad9.net:853"}
	if len(forward.Upstreams) != len(want) {
		t.Fatalf("the forward names %d upstreams %v, want %v", len(forward.Upstreams), addrs(forward), want)
	}
	for i, addr := range want {
		if forward.Upstreams[i].Addr != addr {
			t.Fatalf("upstream %d is %q, want %q: the document's order is the policy's order, "+
				"because a reader comparing the two needs them to be comparable", i, forward.Upstreams[i].Addr, addr)
		}
	}
}

func TestADisabledEntryIsNotRenderedButIsStillInThePolicy(t *testing.T) {
	off := false
	policy := config.Defaults()
	policy.Foreign.Upstreams = append(policy.Foreign.Upstreams, config.ForeignUpstream{
		Kind: config.UpstreamKindUpstream, Name: "spare", Addr: "tls://1.1.1.1:853", Enabled: &off,
	})
	document, err := Render(policy, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, addr := range addrs(foreignForward(t, document)) {
		if addr == "tls://1.1.1.1:853" {
			t.Fatal("a disabled entry was rendered into the forward, so disabling an entry " +
				"does not take it out of the route")
		}
	}
}

// Review Focus #1 in the shape that bites: a udp:// entry may sit in the forward
// list, and the ECH source must still not be it.
func TestAUdpEntryIsRoutableButIsNeverTheECHSource(t *testing.T) {
	off := false
	policy := config.Defaults()
	policy.Foreign.Upstreams = []config.ForeignUpstream{
		{Kind: config.UpstreamKindUpstream, Name: "plain", Addr: "udp://9.9.9.9:53"},
		{Kind: config.UpstreamKindDNSCrypt, Name: "packaged", Enabled: &off},
	}
	// This policy is REFUSED, and that is the point: with the dnscrypt entry off
	// and only a udp entry left there is nothing ECH may be fetched through.
	if err := policy.Validate(); !errors.Is(err, config.ErrNoECHSource) {
		t.Fatalf("a udp-only route with the packaged resolver off was accepted (err = %v), so an "+
			"operator can leave the ECH fetch with no transport at all", err)
	}
	// And with a tcp:// entry added it renders, and the source is the tcp entry.
	policy.Foreign.Upstreams = append(policy.Foreign.Upstreams, config.ForeignUpstream{
		Kind: config.UpstreamKindUpstream, Name: "stream", Addr: "tcp://9.9.9.9:5353",
	})
	document, err := Render(policy, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := cdnRewrite(t, document).ForeignUpstream; got != "tcp://9.9.9.9:5353" {
		t.Fatalf("the ECH source is %q, want the tcp:// entry's addr: the udp entry is routable "+
			"but cdn_rewrite refuses every non-tcp transport for the ECH fetch", got)
	}
}

// **The assertion the spec asks for by name.** scanForChinesePublicDNS already
// scans the WHOLE finished document (render.go:743), so a new upstream entry that
// renders into that document is covered without a line of new code. "It is covered
// automatically" is a claim about code nobody changed, and the only thing that
// makes it more than a claim is a case that puts a Chinese resolver where a new
// entry can put one and refuses the render.
func TestAChineseResolverIsRefusedWhereverAnUpstreamEntryPutsIt(t *testing.T) {
	for _, chinese := range []string{"223.5.5.5", "223.6.6.6", "119.29.29.29", "114.114.114.114", "180.76.76.76"} {
		for _, addr := range []string{"https://" + chinese + "/dns-query", "quic://dns.quad9.net:853"} {
			policy := config.Defaults()
			policy.Foreign.Upstreams = append(policy.Foreign.Upstreams, config.ForeignUpstream{
				Kind: config.UpstreamKindUpstream, Name: "extra", Addr: addr,
			})
			_, err := Render(policy, ProductionPaths())
			if addr == "https://"+chinese+"/dns-query" {
				if err == nil {
					t.Errorf("%s was rendered into the document, so an operator can point "+
						"the foreign branch at a resolver this package exists to avoid", chinese)
				}
				continue
			}
			if err != nil {
				t.Errorf("a clean entry was refused alongside a %s case: %v", chinese, err)
			}
		}
	}
	// And the bootstrap list is scanned too, which is the place a Chinese resolver
	// would actually appear by accident: an operator who set a bootstrap to
	// 223.5.5.5 has made the machine resolve this router's own upstream names in
	// China, and the rendered document is where that becomes visible.
	policy := config.Defaults()
	policy.Foreign.Upstreams[1].Bootstrap = []string{"223.5.5.5:53"}
	if _, err := Render(policy, ProductionPaths()); err == nil {
		t.Error("a bootstrap pointing at 223.5.5.5 was rendered, so the one place a Chinese " +
			"resolver would arrive by accident is unchecked")
	}
}

func TestConcurrentIsWrittenIntoTheForward(t *testing.T) {
	for _, value := range []int{1, 2, 3} {
		policy := config.Defaults()
		policy.Foreign.Concurrent = value
		document, err := Render(policy, ProductionPaths())
		if err != nil {
			t.Fatalf("Render with concurrent=%d: %v", value, err)
		}
		if got := foreignForward(t, document).Concurrent; got != value {
			t.Fatalf("the forward says concurrent = %d, want %d: at 1 mosdns picks RANDOMLY "+
				"(forward.go:265-267) and an operator who asked for two upstreams would have "+
				"neither redundancy nor a document that says so", got, value)
		}
	}
}

func TestTheAPIListenerIsLoopbackAndPresent(t *testing.T) {
	document, err := Render(config.Defaults(), ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got document
	if err := yaml.UnmarshalWithOptions([]byte(document), &got, yaml.Strict()); err != nil {
		t.Fatalf("the rendered document does not decode strictly: %v", err)
	}
	if got.API.HTTP != apiListenAddress {
		t.Fatalf("api.http is %q, want %q: without it the cache plugin's own flush endpoint "+
			"(mosdns cache.go:320-324, mounted unconditionally at :108) is unreachable",
			got.API.HTTP, apiListenAddress)
	}
	if !strings.HasPrefix(got.API.HTTP, "127.0.0.1:") {
		t.Fatalf("api.http is %q, which is not loopback; this package's promise is that every "+
			"listener it configures is loopback-only", got.API.HTTP)
	}
}
```

**辅助函数**（本文件已有 `rendered` 严格解码类型，`:59-62`；给它加 `API` 字段，并加这三个读取器）：

```go
// document gains the api block, because the renderer now emits one and the strict
// decode would otherwise fail on it -- which is the decode doing its job.
type document struct {
	Log     logArgs      `yaml:"log"`
	Plugins []pluginArgs `yaml:"plugins"`
	API     apiArgs      `yaml:"api"`
}

type apiArgs struct {
	HTTP string `yaml:"http"`
}

func foreignForward(t *testing.T, text string) forwardBlock {
	t.Helper()
	for _, plugin := range pluginsOf(t, text) {
		if plugin.Tag == tagForeignForward {
			var out forwardBlock
			decodeInto(t, plugin.Args, &out)
			return out
		}
	}
	t.Fatalf("the document has no %s plugin", tagForeignForward)
	return forwardBlock{}
}

type forwardBlock struct {
	Concurrent int `yaml:"concurrent"`
	Upstreams  []struct {
		Addr      string `yaml:"addr"`
		Bootstrap string `yaml:"bootstrap"`
	} `yaml:"upstreams"`
}

func cdnRewrite(t *testing.T, text string) cdnRewriteBlock { /* 同上，找 tagCDNRewrite */ }

type cdnRewriteBlock struct {
	ForeignUpstream string `yaml:"foreign_upstream"`
}

func addrs(f forwardBlock) []string {
	out := make([]string, 0, len(f.Upstreams))
	for _, u := range f.Upstreams {
		out = append(out, u.Addr)
	}
	return out
}
```

- [ ] **Step 6: 运行，确认它红**

```
go test ./internal/mosdnsconfig/ -run 'TestEveryEnabledUpstream|TestADisabledEntryIsNot|TestAUdpEntryIs|TestConcurrentIsWritten|TestTheAPIListenerIs' -v
```
Expected: 编译失败（`apiListenAddress` 未定义、`document` 没有 `API`）。

- [ ] **Step 7: 渲染 `api` 块与 `concurrent`**

`internal/mosdnsconfig/render.go`：

```go
// document 加一个字段。cache 插件自己注册了 flush 端点
// （mosdns plugin/executable/cache/cache.go:320-324，在 :108 无条件 mount 到
// /plugins/<tag>），而服务端只在配置带 api.http 时才起（coremain/mosdns.go:67）。
// 这个包今天不输出 api 键，所以那个端点够不着。
type document struct {
	Log     logArgs      `yaml:"log"`
	Plugins []pluginArgs `yaml:"plugins"`
	API     apiArgs      `yaml:"api"`
}

type apiArgs struct {
	HTTP string `yaml:"http"`
}

// apiListenAddress 是 flush 端点的 loopback 地址。loopback，因为这个包的承诺是
// 它配置的每一个 listener 都只绑 loopback；非特权，因为路由器绑的是 53。
const apiListenAddress = "127.0.0.1:15354"
```

`forwardArgs` 与 `upstreamArgs`：

```go
	// forwardArgs is the foreign forward. Concurrent is how many upstreams ONE
	// query races; at 1 mosdns picks RANDOMLY (forward.go:265-267), which is load
	// balancing and not redundancy, so the policy states it rather than leaving it
	// to a default an operator cannot see.
	forwardArgs struct {
		Concurrent int             `yaml:"concurrent"`
		Upstreams  []upstreamArgs `yaml:"upstreams"`
	}
	upstreamArgs struct {
		Addr string `yaml:"addr"`
		// Bootstrap resolves THIS upstream's own host name. The machine's only
		// resolver is this router, so a domain upstream without one cannot be
		// dialled at all. mosdns takes it as a comma-separated string
		// (forward.UpstreamConfig.Bootstrap, a string), so that is the form here.
		Bootstrap string `yaml:"bootstrap"`
	}
```

- [ ] **Step 8: 在 `Render` 里算出并渲染**

在 `resolve()` 返回的 `resolved` 结构上加两个字段（`resolvedPaths`）：

```go
	// echUpstream is the ONE address the ECH key is fetched through, derived from
	// the policy's enabled entries and resolved here so the routing document and
	// the rewriter's argument cannot disagree.
	echUpstream string
	// forwardUpstreams is the enabled `upstream` entries, in policy order.
	forwardUpstreams []upstreamArgs
```

在 `resolve()` 的 `checkForeignListener` 之后插入（**替换**原来那段「ForeignListener 是唯一上游」的假设）：

```go
	// The foreign route comes from the policy now, and the DNSCrypt listener is one
	// ENTRY in it rather than a constant this renderer owns.
	//
	// Paths.ForeignListener is still read, and still WINS when it is set, because it
	// is the injection seam the integration tests use to point the document at a mock
	// resolver (tests/integration/routing_test.go:457). ProductionPaths() leaves it
	// empty, so production derives the listener from the dnscrypt entry -- and from
	// dnscrypt.ListenAddress(), not from a second copy of the string.
	resolved.forwardUpstreams = nil
	for _, entry := range policy.Foreign.Upstreams {
		if !entry.IsEnabled() || entry.Kind != config.UpstreamKindUpstream {
			continue
		}
		resolved.forwardUpstreams = append(resolved.forwardUpstreams, upstreamArgs{
			Addr:      entry.Addr,
			Bootstrap: strings.Join(entry.Bootstrap, ","),
		})
	}
	if len(resolved.forwardUpstreams) == 0 {
		return resolvedPaths{}, fmt.Errorf("mosdnsconfig: the policy enables no upstream entry, so the " +
			"foreign branch has nowhere to send a query")
	}
	listener := strings.TrimSpace(paths.ForeignListener)
	if listener == "" {
		// The dnscrypt entry's address is the packaged resolver's own, and the
		// ECH source is the first enabled tcp:// entry when there is no dnscrypt
		// entry -- which is what config.ECHSource decides, called here with the
		// listener it will actually be given rather than with a guess.
		if source := config.ECHSource(policy.Foreign.Upstreams, "tcp://"+dnscrypt.ListenAddress); source != "" {
			if _, err := checkForeignListener(source); err != nil {
				return resolvedPaths{}, err
			}
			listener = source
		}
	}
	resolved.ForeignListener = listener
	resolved.echUpstream = config.ECHSource(policy.Foreign.Upstreams, listener)
	if resolved.echUpstream == "" {
		return resolvedPaths{}, config.ErrNoECHSource
	}
```

然后在 `renderable()` 里：

```go
			{
				Tag:  tagForeignCache,
				Type: "cache",
				Args: cacheArgs{Size: policy.ForeignCache.Size},
			},
			{
				Tag:  tagForeignForward,
				Type: "forward",
				Args: forwardArgs{
					Concurrent: policy.Foreign.Concurrent,
					Upstreams:  resolved.forwardUpstreams,
				},
			},
```

以及把 `cdnRewriteArgs.ForeignUpstream` 从 `paths.ForeignListener` 改成 `resolved.echUpstream`（注释同步改成「推导出来的，不是常量」）。

**`api` 块怎么进 YAML——只有一条路，别走捷径。** `render.go:196` 的注释写死了这个仓的纪律：「文档由有类型的值组装然后一次性 marshal，绝不字符串拼接」。所以：

1. `document` 结构加 `API apiArgs`（Step 7 已给出）；
2. `renderable()` 的返回类型从 `[]pluginArgs` 改成 `document`，把 `Log`、`Plugins`、`API` 一起返回；
3. `marshal()` 接受 `document` 而不是 `[]pluginArgs`；
4. `Render()` 里 `encoded, err := marshal(renderable(policy, resolved))` 不变。

**不要在 marshal 之后手工补三行 YAML。** 那会让 `api.http` 成为整份文档里唯一一个不是由类型生成的键，而 `TestTheAPIListenerIsLoopbackAndPresent` 用的是严格解码——它能过，但下一次有人改 `apiArgs` 的 yaml tag 时不会有任何测试发现文档和代码对不上了。

- [ ] **Step 9: 运行，确认它绿**

```
go test ./internal/mosdnsconfig/ -run 'TestEveryEnabledUpstream|TestADisabledEntryIsNot|TestAUdpEntryIs|TestConcurrentIsWritten|TestTheAPIListenerIs' -v
```
Expected: PASS。

- [ ] **Step 10: 改写 §5.1 里被打破的九处断言**

逐条改，**不是删**：

| 现有测试 | 改成 |
|---|---|
| `TestTheForeignForwardUsesOnlyTheDNSCryptListenerOverTCP` `:606` | 删掉 `len != 1` 的断言，换成：每个启用的 `upstream` 条目产生恰好一个 upstream、顺序与策略一致，且**没有条目产生非 tcp 的 ECH 来源** |
| `TestTheECHKeyIsFetchedThroughTheSameListenerTheForeignBranchDials` `:568` | 保留「两者相等」；`len != 1` 换成「forward 至少一个 upstream 且 `foreign_upstream` 是其中之一或是 dnscrypt listener」；**新增一个对照**：dnscrypt 条目禁用时 ECH 来源落到第一条 tcp 条目 |
| `TestTheForeignListenerIsWhereTheCommittedDNSCryptProxyListens` `:941` | 保留；新增「`dnscrypt.ListenAddress()` 就是 toml 里那个地址」 |
| `TestCommittedMosdnsConfigIsExactlyTheRenderedDefault` `:905` | **不动** |
| `TestRenderedRoutingNamesEachPluginExactlyOnceInLoadOrder` `:179` | 测试侧 `rendered` 类型加 `API`；插件表在夹紧生效时多一个 `foreign_ttl` |
| `TestForeignPathAnswersFromTheCacheBeforeItForwards` `:406` | 保留原样 |
| `TestRenderedConfigLoadsInAMosdnsInstance` `:986` | 「恰好 1 次 TCP 0 次 UDP」改成「两个上游都被配置，且答案来自先应答的那个」 |
| `TestRenderRefusesListenersThatWouldCollide` `:812` | 保留；新增「没有 ECH 来源的策略被拒绝」 |
| `TestNoAddressButLoopbackIsCommitted` `:970` | **不动**（自动覆盖 bootstrap 与 15354） |

- [ ] **Step 11: 重新生成 `configs/mosdns.yaml` 并跑整个包**

```
MOSDNS_ROUTER_UPDATE=1 go test ./internal/mosdnsconfig/ -run TestCommittedMosdnsConfig -v
go test ./internal/mosdnsconfig/ -v
```
Expected: 全 PASS。

- [ ] **Step 12: 变异验证**

```bash
cp internal/mosdnsconfig/render.go /home/ubuntu/.gotmp-mosdns/render.bak
# a. ECH 来源直接用第一条 upstream 而不是推导
python3 - <<'PY'
from pathlib import Path
p=Path('internal/mosdnsconfig/render.go'); t=p.read_text()
t=t.replace('resolved.echUpstream = config.ECHSource(policy.Foreign.Upstreams, listener)',
            'resolved.echUpstream = resolved.forwardUpstreams[0].Addr')
p.write_text(t)
PY
go test ./internal/mosdnsconfig/ -run 'TestAUdpEntryIs|TestTheECHKeyIsFetched' 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render.bak internal/mosdnsconfig/render.go
# b. disabled 条目也渲染
python3 - <<'PY'
from pathlib import Path
p=Path('internal/mosdnsconfig/render.go'); t=p.read_text()
t=t.replace('if !entry.IsEnabled() || entry.Kind != config.UpstreamKindUpstream {','if entry.Kind != config.UpstreamKindUpstream {')
p.write_text(t)
PY
go test ./internal/mosdnsconfig/ -run TestADisabledEntryIsNot 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render.bak internal/mosdnsconfig/render.go
# c. api 块不渲染
python3 - <<'PY'
from pathlib import Path
p=Path('internal/mosdnsconfig/render.go'); t=p.read_text()
t=t.replace('HTTP: apiListenAddress,','')
p.write_text(t)
PY
go test ./internal/mosdnsconfig/ -run TestTheAPIListenerIs 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render.bak internal/mosdnsconfig/render.go
go test ./internal/mosdnsconfig/ 2>&1 | tail -2   # 期望 ok
```

- [ ] **Step 13: 提交**

```bash
git add internal/dnscrypt/ internal/mosdnsconfig/ configs/mosdns.yaml
git commit -m "feat(mosdnsconfig): the routing document carries the policy's upstreams

foreign_forward gains a list and a concurrent count, cdn_rewrite's
foreign_upstream becomes DERIVED rather than constant, and the document grows the
api block the cache plugin's own flush endpoint has always needed.

The ECH source is the part worth reading. It used to be Paths.ForeignListener, a
Go constant, and the renderer wrote the same value into two places so the two
could not disagree. Now it is config.ECHSource over the policy's enabled entries:
the dnscrypt entry's listener when that entry is on, otherwise the first enabled
tcp:// entry. A udp:// entry is routable and is never the ECH source, which is
TestAUdpEntryIsRoutableButIsNeverTheECHSource and it is the shape of the mistake
an operator makes when they add a DoH upstream and switch the packaged resolver
off: the policy is refused by name rather than rendered into a document where
every forced-ECH name fails closed once the grace ends.

dnscrypt.ListenAddress is exported so the routing renderer reads the SAME string
the resolver document writes. A second copy is how a test asserting the two
documents agree comes to pass while they do not.

Paths.ForeignListener still wins when set, because it is the injection seam the
integration tests use (tests/integration/routing_test.go:457);
ProductionPaths() leaves it empty so production derives the listener.

The committed document is regenerated and TestCommittedMosdnsConfigIsExactlyThe
RenderedDefault still holds byte for byte.

Mutation-verified: deriving the ECH source from the first upstream rather than
ECHSource, rendering disabled entries, and dropping the api block each turn the
matching case red."
```

---

### Task 3: `check-upstream`

**Files:**
- Create: `cmd/mosdns-cdnctl/check_upstream.go`
- Create: `cmd/mosdns-cdnctl/check_upstream_test.go`
- Modify: `cmd/mosdns-cdnctl/update_lists.go:55-83`（`services` 加 `newUpstream`）、`:104-124`（`productionServices`）
- Modify: `cmd/mosdns-cdnctl/main.go:66-92`（dispatch 表）

**Interfaces:**
- Consumes: `config.ForeignUpstream`、`config.IsEnabled`（Task 1）
- Produces:
  ```go
  // cmd/mosdns-cdnctl/update_lists.go
  // newUpstream builds a dialable upstream. It is a FIELD so a test can hand the
  // verb an upstream that answers, refuses, or never answers, without a network.
  newUpstream func(addr string, opt upstream.Opt) (upstream.Upstream, error)

  // cmd/mosdns-cdnctl/check_upstream.go
  const checkUpstreamProbeName = "example.com"

  type upstreamCheckResult struct {
      Name    string
      Kind    config.UpstreamKind
      Target  string
      Rcode   string
      Elapsed time.Duration
      Err     error
  }

  func runCheckUpstream(args []string, stdout, stderr io.Writer, services services) int
  func checkOneUpstream(ctx context.Context, u config.ForeignUpstream, dnscryptListener string, build upstreamBuilder, now func() time.Time) upstreamCheckResult
  ```

- [ ] **Step 1: 写失败的测试——输出格式与退出码**

`cmd/mosdns-cdnctl/check_upstream_test.go`：

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/pool"
	"github.com/IrineSistiana/mosdns/v5/pkg/upstream"
	"github.com/miekg/dns"

	"mosdns-router/internal/config"
)

// One line per ENABLED entry, in policy order, whether it answered or not -- so a
// report that names four entries and describes two is visibly incomplete rather
// than quietly shorter.
func TestTheReportNamesEveryEnabledEntryInPolicyOrder(t *testing.T) {
	services := checkServices(t, map[string]checkAnswer{
		"tcp://127.0.0.1:15353": {rcode: "NOERROR"},
		"quic://dns.quad9.net:853": {rcode: "NOERROR"},
	})
	var stdout, stderr bytes.Buffer
	code := runCheckUpstream([]string{"--all"}, &stdout, &stderr, services)

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("the report has %d lines, want one per enabled entry:\n%s", len(lines), stdout.String())
	}
	if !strings.HasPrefix(lines[0], "quad9-dnscrypt:") || !strings.HasPrefix(lines[1], "quad9-doq:") {
		t.Fatalf("the report is not in policy order:\n%s", stdout.String())
	}
	if !strings.Contains(lines[0], "tcp://127.0.0.1:15353") {
		t.Fatalf("the dnscrypt line does not name the address it checked:\n%s", lines[0])
	}
	if code != exitSuccess {
		t.Fatalf("exit %d with every entry reachable, want 0\n%s", code, stderr.String())
	}
}

// The exit code is about REACHABILITY, so an operator scripting it can tell
// "everything is up" from "something is down" without parsing the report.
func TestAnUnreachableEntryIsNamedAndChangesTheExitCode(t *testing.T) {
	services := checkServices(t, map[string]checkAnswer{
		"tcp://127.0.0.1:15353": {rcode: "NOERROR"},
		"quic://dns.quad9.net:853": {err: errors.New("dial udp: connection refused")},
	})
	var stdout, stderr bytes.Buffer
	code := runCheckUpstream([]string{"--all"}, &stdout, &stderr, services)

	if code != exitStateUnavailable {
		t.Fatalf("exit %d with one entry unreachable, want %d", code, exitStateUnavailable)
	}
	if !strings.Contains(stdout.String(), "quad9-doq") {
		t.Fatalf("the failing entry is not named:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "connection refused") {
		t.Fatalf("the failure reason is not carried:\n%s", stdout.String())
	}
	// And the reachable one is still reported: a report that stops at the first
	// failure tells the operator nothing about the rest.
	if !strings.Contains(stdout.String(), "quad9-dnscrypt") {
		t.Fatalf("the reachable entry is missing from a report that hit a failure:\n%s", stdout.String())
	}
}

// A DISABLED entry is not checked, because "is this reachable" for something the
// router does not use is a question with no consequence.
func TestADisabledEntryIsNotChecked(t *testing.T) {
	policy := config.Defaults()
	off := false
	policy.Foreign.Upstreams = append(policy.Foreign.Upstreams, config.ForeignUpstream{
		Kind: config.UpstreamKindUpstream, Name: "spare", Addr: "tls://1.1.1.1:853", Enabled: &off,
	})
	var built []string
	services := checkServices(t, map[string]checkAnswer{})
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }
	services.newUpstream = func(addr string, _ upstream.Opt) (upstream.Upstream, error) {
		built = append(built, addr)
		return &checkFake{rcode: "NOERROR"}, nil
	}
	var stdout, stderr bytes.Buffer
	runCheckUpstream([]string{"--all"}, &stdout, &stderr, services)
	for _, addr := range built {
		if strings.Contains(addr, "1.1.1.1") {
			t.Fatalf("a disabled entry was dialled (%v)", built)
		}
	}
}

// Review Focus #4: one unresolvable name must produce ONE named failure, not a
// whole-run failure and not a whole-run success.
func TestOneUnresolvableNameFailsOnlyItsOwnEntry(t *testing.T) {
	services := checkServices(t, map[string]checkAnswer{
		"tcp://127.0.0.1:15353": {rcode: "NOERROR"},
	})
	policy := config.Defaults()
	policy.Foreign.Upstreams = []config.ForeignUpstream{
		{Kind: config.UpstreamKindDNSCrypt, Name: "packaged"},
		{Kind: config.UpstreamKindUpstream, Name: "doq", Addr: "quic://dns.quad9.net:853"},
	}
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }
	services.newUpstream = func(addr string, _ upstream.Opt) (upstream.Upstream, error) {
		if strings.Contains(addr, "dns.quad9.net") {
			// The bootstrap is what makes this address resolvable at all; a
			// misconfigured bootstrap shows up here and nowhere else.
			return nil, errors.New("bootstrap: no resolver answered for dns.quad9.net")
		}
		return &checkFake{rcode: "NOERROR"}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runCheckUpstream([]string{"--all"}, &stdout, &stderr, services)
	if code != exitStateUnavailable {
		t.Fatalf("exit %d, want non-zero: an entry that cannot be dialled is not reachable", code)
	}
	if !strings.Contains(stdout.String(), "doq") || !strings.Contains(stdout.String(), "packaged") {
		t.Fatalf("both entries should be reported:\n%s", stdout.String())
	}
}

// The probe name is IANA's, and that is a decision this test holds: it is the
// name that must resolve everywhere, so a failure is the transport and not a
// provider's policy.
func TestTheProbeNameIsTheOneReservedForThis(t *testing.T) {
	if checkUpstreamProbeName != "example.com" {
		t.Fatalf("the probe name is %q; it must be a name every resolver answers, so that a "+
			"failure is the transport rather than one provider's filtering", checkUpstreamProbeName)
	}
}

func TestNamingOneEntryChecksOnlyThatOne(t *testing.T) {
	services := checkServices(t, map[string]checkAnswer{
		"tcp://127.0.0.1:15353": {rcode: "NOERROR"},
	})
	var stdout, stderr bytes.Buffer
	code := runCheckUpstream([]string{"quad9-dnscrypt"}, &stdout, &stderr, services)
	if code != exitSuccess {
		t.Fatalf("exit %d checking one reachable entry: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "quad9-doq") {
		t.Fatalf("naming one entry checked another:\n%s", stdout.String())
	}
}

func TestNamingAnEntryThatIsNotThereIsAUsageError(t *testing.T) {
	services := checkServices(t, map[string]checkAnswer{})
	var stdout, stderr bytes.Buffer
	if code := runCheckUpstream([]string{"nope"}, &stdout, &stderr, services); code != exitInvalidCLI {
		t.Fatalf("exit %d naming an entry that does not exist, want %d", code, exitInvalidCLI)
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Fatalf("the refusal does not name what was asked for: %s", stderr.String())
	}
}
```

**本文件的夹具**：

```go
type checkAnswer struct {
	rcode string
	err   error
}

// checkFake is an upstream that answers what the fixture said, and records the
// question it was asked so a case can assert the probe's TYPE as well as its name.
type checkFake struct {
	mu    sync.Mutex
	rcode string
	err   error
	asked []dns.Question
	closed bool
}

func (f *checkFake) ExchangeContext(_ context.Context, payload []byte) (*[]byte, error) {
	var query dns.Msg
	if err := query.Unpack(payload); err != nil {
		return nil, err
	}
	if len(query.Question) == 0 {
		return nil, errors.New("a query with no question")
	}
	f.mu.Lock()
	f.asked = append(f.asked, query.Question[0])
	failure := f.err
	f.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	response := new(dns.Msg)
	response.SetReply(&query)
	response.Rcode = dns.StringToRcode[f.rcode]
	// pool.PackBuffer (pkg/pool/msg_buf.go:37) is mosdns's OWN get-a-buffer,
	// pack, copy-into-a-right-sized-buffer helper, and the interface hands back a
	// pooled *[]byte. Writing that sequence by hand in a fixture is four lines of
	// buffer lifetime to get wrong for no reason.
	return pool.PackBuffer(response)
}

func (f *checkFake) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }

func checkServices(t *testing.T, answers map[string]checkAnswer) services {
	t.Helper()
	value := services{
		loadPolicy:  func(string) (config.Policy, error) { return config.Defaults(), nil },
		documents:   productionDocumentPaths(),
		now:         func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) },
		newUpstream: func(addr string, _ upstream.Opt) (upstream.Upstream, error) {
			answer, ok := answers[addr]
			if !ok {
				return nil, fmt.Errorf("%s was dialled and the fixture has no answer for it", addr)
			}
			if answer.err != nil {
				return nil, answer.err
			}
			return &checkFake{rcode: answer.rcode}, nil
		},
	}
	return value
}
```

- [ ] **Step 2: 运行，确认它红**

```
go test ./cmd/mosdns-cdnctl/ -run 'TestTheReportNames|TestAnUnreachableEntry|TestADisabledEntryIsNot|TestOneUnresolvableName|TestTheProbeNameIs|TestNamingOneEntry|TestNamingAnEntryThatIsNot' -v
```
Expected: 编译失败，`undefined: runCheckUpstream`。

- [ ] **Step 3: 给 `services` 加两个注入点**

`cmd/mosdns-cdnctl/update_lists.go`，在 `services` 结构里加：

```go
	// loadPolicy reads the policy the route is configured in. It is a field so the
	// connectivity check can be handed a policy that is not the installed one,
	// which is the only way to test "an operator added an entry" without writing
	// to /etc on the machine the tests run on.
	loadPolicy func(path string) (config.Policy, error)
	// newUpstream builds a dialable upstream. It is a field so the connectivity
	// check can be handed an upstream that answers, refuses, or never answers,
	// without a network and without a DNS server.
	newUpstream func(addr string, opt upstream.Opt) (upstream.Upstream, error)
```

`productionServices()` 里加：

```go
		loadPolicy: config.Load,
		newUpstream: func(addr string, opt upstream.Opt) (upstream.Upstream, error) {
			return upstream.NewUpstream(addr, opt)
		},
```

- [ ] **Step 4: 写 `check_upstream.go`**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/pool"
	"github.com/IrineSistiana/mosdns/v5/pkg/upstream"
	"github.com/miekg/dns"
	"go.uber.org/zap"

	"mosdns-router/internal/config"
	"mosdns-router/internal/dnscrypt"
)

// checkUpstreamProbeName is the name every entry is asked about.
//
// **example.com, and that is a decision rather than a default.** It is IANA's
// reserved example domain and every recursive resolver is required to answer it, so
// a failure is the transport or the reachability and not one provider's filtering
// or one zone's policy. Asking a Cloudflare name or a Google name would mix "this
// resolver refuses example.com" into the same line as "this network cannot reach
// it", and an operator reading the report would have to guess which.
const checkUpstreamProbeName = "example.com"

// checkUpstreamTimeout bounds ONE entry's probe. It is a check an operator is
// watching, so it answers in seconds rather than in the router's own 5-second
// per-query budget plus retries.
const checkUpstreamTimeout = 5 * time.Second

// runCheckUpstream reports whether each enabled entry in the policy's foreign route
// can actually be dialled and can answer.
//
// It exists because "is my DNS working" has two very different answers -- the
// router is fine and one upstream is unreachable, or every upstream is unreachable
// -- and an operator has no way to tell them apart from the router's behaviour: both
// look like "foreign names do not resolve". This verb asks each entry directly.
//
// Exit status is about REACHABILITY and nothing else: 0 when every enabled entry
// answered, 3 when at least one did not. It never writes and never locks.
func runCheckUpstream(args []string, stdout, stderr io.Writer, services services) int {
	flags := flag.NewFlagSet("check-upstream", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	all := flags.Bool("all", false, "check every enabled entry")
	policyPath := flags.String("policy", "", "path to the policy the route is configured in")
	var names []string
	if err := flags.Parse(args); err != nil {
		writeCLIError(stderr, "check-upstream: %v", err)
		return exitInvalidCLI
	}
	if flags.NArg() > 0 {
		names = append(names, flags.Args()...)
	}
	if !*all && len(names) == 0 {
		writeCLIError(stderr, "check-upstream: name at least one entry, or pass --all")
		return exitInvalidCLI
	}
	if *all && len(names) > 0 {
		writeCLIError(stderr, "check-upstream: --all and a name are two different questions")
		return exitInvalidCLI
	}

	policy, err := services.loadPolicy(resolvePolicyPath(*policyPath, services))
	if err != nil {
		writeCLIError(stderr, "check-upstream: %v", err)
		return exitStateUnavailable
	}
	listener := "tcp://" + dnscrypt.ListenAddress
	selected, missing := selectUpstreams(policy.Foreign.Upstreams, names)
	if len(missing) > 0 {
		// Refused BEFORE anything is dialled, and it names what was not found AND
		// what is there -- an operator who typed a name wrong should not have to
		// run the command again to find the right one.
		writeCLIError(stderr, "check-upstream: no enabled entry named %s. The enabled entries are: %s",
			strings.Join(missing, ", "), strings.Join(enabledNames(policy.Foreign.Upstreams), ", "))
		return exitInvalidCLI
	}
	if len(selected) == 0 {
		writeCLIError(stderr, "check-upstream: the policy enables no upstream entry, so there is "+
			"nothing to check; that is a configuration fault rather than a route that is down")
		return exitStateUnavailable
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkUpstreamTimeout*time.Duration(len(selected)+1))
	defer cancel()

	unreachable := 0
	for _, entry := range selected {
		result := checkOneUpstream(ctx, entry, listener, services.newUpstream, services.now)
		writeReportLine(stdout, "%s\n", formatUpstreamCheck(result))
		if result.Err != nil {
			unreachable++
		}
	}
	if unreachable > 0 {
		writeCLIError(stderr, "check-upstream: %d of %d enabled entries did not answer %s",
			unreachable, len(selected), checkUpstreamProbeName)
		return exitStateUnavailable
	}
	return exitSuccess
}

// selectUpstreams is the whole of "which entries", and it returns the MISSING
// names alongside what it found. An operator who typed a name wrong must be told,
// not shown a clean report about the entries that happened to match.
//
// **The missing list is returned rather than raised, and that is deliberate.** A
// first version raised here, and the caller then iterated the selection without
// noticing the error -- so a typo produced a successful, shorter report and exit 0.
// Two functions that each hold half of one decision is how that happened, so the
// whole decision is one return value.
func selectUpstreams(upstreams []config.ForeignUpstream, names []string) (selected []config.ForeignUpstream, missing []string) {
	enabled := make([]config.ForeignUpstream, 0, len(upstreams))
	for _, entry := range upstreams {
		if entry.IsEnabled() {
			enabled = append(enabled, entry)
		}
	}
	if len(names) == 0 {
		return enabled, nil
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for _, entry := range enabled {
		if wanted[entry.Name] {
			selected = append(selected, entry)
			delete(wanted, entry.Name)
		}
	}
	for name := range wanted {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return selected, missing
}

func checkOneUpstream(ctx context.Context, entry config.ForeignUpstream, dnscryptListener string,
	build func(string, upstream.Opt) (upstream.Upstream, error), now func() time.Time) upstreamCheckResult {

	target := entry.Addr
	if entry.Kind == config.UpstreamKindDNSCrypt {
		// The packaged listener, over TCP -- the SAME road the ECH key is fetched
		// through (cdn_rewrite.go:353 refuses every other transport). So this line
		// is also the answer to "can this machine still fetch an ECH key".
		target = dnscryptListener
	}
	result := upstreamCheckResult{Name: entry.Name, Kind: entry.Kind, Target: target}

	dialer, err := build(target, upstream.Opt{Logger: zap.NewNop()})
	if err != nil {
		result.Err = err
		return result
	}
	defer func() { _ = dialer.Close() }()

	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(checkUpstreamProbeName), dns.TypeA)
	payload, err := query.Pack()
	if err != nil {
		result.Err = err
		return result
	}
	attempt, cancel := context.WithTimeout(ctx, checkUpstreamTimeout)
	defer cancel()

	started := now()
	answer, err := dialer.ExchangeContext(attempt, payload)
	result.Elapsed = now().Sub(started)
	if err != nil {
		result.Err = err
		return result
	}
	pool.ReleaseBuf(answer)
	response := new(dns.Msg)
	if err := response.Unpack(*answer); err != nil {
		result.Err = fmt.Errorf("read its answer: %w", err)
		return result
	}
	result.Rcode = dns.RcodeToString[response.Rcode]
	if response.Rcode != dns.RcodeSuccess {
		// A reachable resolver that REFUSES is not an unreachable one, and the two
		// need different operator answers, so they are different lines.
		result.Err = fmt.Errorf("it answered %s", result.Rcode)
	}
	return result
}

func formatUpstreamCheck(r upstreamCheckResult) string {
	var out strings.Builder
	out.WriteString(r.Name)
	out.WriteString(": ")
	out.WriteString(string(r.Kind))
	out.WriteString(" ")
	out.WriteString(r.Target)
	out.WriteString(": ")
	if r.Err != nil {
		out.WriteString("UNREACHABLE after ")
		out.WriteString(r.Elapsed.Round(time.Millisecond).String())
		out.WriteString(" -- ")
		out.WriteString(r.Err.Error())
		return out.String()
	}
	out.WriteString(r.Rcode)
	out.WriteString(" in ")
	out.WriteString(r.Elapsed.Round(time.Millisecond).String())
	return out.String()
}

// enabledNames is the list the "no entry named X" refusal prints, so the operator
// is handed the right name rather than a second attempt.
func enabledNames(upstreams []config.ForeignUpstream) []string {
	var out []string
	for _, entry := range upstreams {
		if entry.IsEnabled() {
			out = append(out, entry.Name)
		}
	}
	return out
}

func resolvePolicyPath(flagValue string, services services) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return services.documents.Policy
}
```

并在 `main.go` 的 dispatch 表加：

```go
	case "check-upstream":
		return runCheckUpstream(args[1:], stdout, stderr, services)
```

- [ ] **Step 5: 运行，确认它绿**

```
go test ./cmd/mosdns-cdnctl/ -run 'TestTheReportNames|TestAnUnreachableEntry|TestADisabledEntryIsNot|TestOneUnresolvableName|TestTheProbeNameIs|TestNamingOneEntry|TestNamingAnEntryThatIsNot' -v
```
Expected: PASS。

- [ ] **Step 6: 变异验证**

```bash
cp cmd/mosdns-cdnctl/check_upstream.go /home/ubuntu/.gotmp-mosdns/cu.bak
# a. 跳过 disabled 条目
sed -i 's/if entry.IsEnabled() {/if true {/' cmd/mosdns-cdnctl/check_upstream.go
go test ./cmd/mosdns-cdnctl/ -run TestADisabledEntryIsNot 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cu.bak cmd/mosdns-cdnctl/check_upstream.go
# b. 失败时仍退出 0
sed -i 's/return exitStateUnavailable$/return exitSuccess/' cmd/mosdns-cdnctl/check_upstream.go
go test ./cmd/mosdns-cdnctl/ -run 'TestAnUnreachableEntry|TestOneUnresolvableName' 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cu.bak cmd/mosdns-cdnctl/check_upstream.go
# c. 探测名换成别的
sed -i 's/const checkUpstreamProbeName = "example.com"/const checkUpstreamProbeName = "www.cloudflare.com"/' cmd/mosdns-cdnctl/check_upstream.go
go test ./cmd/mosdns-cdnctl/ -run TestTheProbeNameIs 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cu.bak cmd/mosdns-cdnctl/check_upstream.go
go test ./cmd/mosdns-cdnctl/ 2>&1 | tail -2   # 期望 ok
```

- [ ] **Step 7: 提交**

```bash
git add cmd/mosdns-cdnctl/
git commit -m "feat(cdnctl): check-upstream asks each entry of the foreign route directly

'is my DNS working' has two very different answers -- the router is fine and one
upstream is unreachable, or every upstream is unreachable -- and from the router's
behaviour they look identical: foreign names simply do not resolve. This verb
asks each enabled entry directly and says which one.

It probes example.com, and that is a decision this commit holds in a test rather
than leaving to a default. example.com is IANA's reserved example domain and every
recursive resolver is required to answer it, so a failure is the transport and not
one provider's filtering. Asking a Cloudflare or Google name would put 'this
resolver refuses example.com' and 'this network cannot reach it' on the same line.

The dnscrypt entry is checked over TCP against the packaged listener, which is the
same road the ECH key is fetched through, so the line also answers 'can this
machine still fetch an ECH key'.

Exit status is reachability and nothing else: 0 when every enabled entry answered,
3 when at least one did not. A reachable resolver that REFUSES is reported as
UNREACHABLE with its rcode, because the two need different operator answers and
one operator action.

Mutation-verified: checking disabled entries, exiting 0 on a failure, and changing
the probe name each turn the matching case red."
```

---

### Task 4: 缓存时间与清空

**Files:**
- Modify: `internal/mosdnsconfig/render.go:44-53`（tag）、`:221-265`（args）、`:355-400`（`renderable`）
- Modify: `internal/mosdnsconfig/render_test.go`
- Create: `cmd/mosdns-cdnctl/flush_cache.go`
- Create: `cmd/mosdns-cdnctl/flush_cache_test.go`
- Modify: `cmd/mosdns-cdnctl/main.go:66-92`

**Interfaces:**
- Consumes: `policy.ForeignCache`（Task 1）、`apiListenAddress`（Task 2）
- Produces:
  ```go
  // internal/mosdnsconfig/render.go
  const tagForeignTTL = "foreign_ttl"

  type ttlArgs struct {
      Max uint32 `yaml:"max,omitempty"`
      Min uint32 `yaml:"min,omitempty"`
  }

  // cmd/mosdns-cdnctl/flush_cache.go
  const foreignCachePluginTag = "foreign_cache"
  func runFlushCache(args []string, stdout, stderr io.Writer, services services) int
  // flushCachePath is where the cache plugin's own endpoint is mounted:
  // coremain/mosdns.go:164-165 mounts a plugin's mux at /plugins/<tag>, and
  // plugin/executable/cache/cache.go:320-324 registers GET /flush on it.
  func flushCachePath() string { return "/plugins/" + foreignCachePluginTag + "/flush" }
  ```

- [ ] **Step 1: 写失败的测试——`ttl_max: 0` 时不渲染插件**

`internal/mosdnsconfig/render_test.go` 追加：

```go
// **The trap this case exists for.** mosdns's ApplyMaximumTTL(m, 0) sets EVERY
// record's TTL to 0 (pkg/dnsutils/msg.go:65-67 -> :94-112) -- zero is not "no bound", it is
// "expire immediately". So a zero max must render NO clamp at all rather than a
// clamp with a zero bound, and it must keep the committed document byte-identical.
func TestATtlClampIsNotRenderedWhenThePolicyAsksForNone(t *testing.T) {
	for _, policy := range []func(*config.Policy){
		func(p *config.Policy) { p.ForeignCache.TTLMax = 0 },
		func(p *config.Policy) { p.ForeignCache.TTLMin = 0 },
		func(p *config.Policy) { p.ForeignCache.TTLMax = 0; p.ForeignCache.TTLMin = 0 },
	} {
		value := config.Defaults()
		policy(&value)
		document, err := Render(value, ProductionPaths())
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if strings.Contains(document, tagForeignTTL) {
			t.Fatalf("a policy asking for no clamp rendered a %s plugin:\n%s", tagForeignTTL, document)
		}
	}
}

func TestATtlClampIsRenderedAfterTheForwardSoTheCacheStoresIt(t *testing.T) {
	value := config.Defaults()
	value.ForeignCache.TTLMax = 300
	document, err := Render(value, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	plugins := pluginTags(t, document)
	forward, ttl := -1, -1
	for index, tag := range plugins {
		switch tag {
		case tagForeignForward:
			forward = index
		case tagForeignTTL:
			ttl = index
		}
	}
	if ttl < 0 {
		t.Fatalf("foreign_cache.ttl_max = 300 rendered no %s plugin:\n%s", tagForeignTTL, document)
	}
	if ttl < forward {
		t.Fatalf("%s is at %d, before %s at %d. The plugin table order IS the next chain, and "+
			"the cache stores whatever its next returned: a clamp before the forward would only "+
			"shape the answer on the way out while the router's own cache still expired on the "+
			"upstream's TTL", tagForeignTTL, ttl, tagForeignForward, forward)
	}
}

// And the min/max keys are omitted rather than written zero, because mosdns reads
// `max: 0` as "clamp everything to 0" (ttl.go:91 guards on `t.max > 0`, so the
// KEY must be absent for the guard to help).
func TestAZeroBoundIsOmittedRatherThanWritten(t *testing.T) {
	value := config.Defaults()
	value.ForeignCache.TTLMax = 120
	value.ForeignCache.TTLMin = 0
	document, err := Render(value, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	block := pluginArgs(t, document, tagForeignTTL)
	if strings.Contains(block, "min:") {
		t.Fatalf("a zero ttl_min was written out, and mosdns would read max=120 with min=0 -- "+
			"which happens to work, but only by accident of the guard order:\n%s", block)
	}
	if !strings.Contains(block, "max: 120") {
		t.Fatalf("ttl_max = 120 was not written:\n%s", block)
	}
}

func TestTheCacheSizeComesFromThePolicy(t *testing.T) {
	value := config.Defaults()
	value.ForeignCache.Size = 77
	document, err := Render(value, ProductionPaths())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(pluginArgs(t, document, tagForeignCache), "size: 77") {
		t.Fatalf("the cache size is not the policy's:\n%s", pluginArgs(t, document, tagForeignCache))
	}
}
```

- [ ] **Step 2: 运行，确认它红**

```
go test ./internal/mosdnsconfig/ -run 'TestATtlClamp|TestAZeroBound|TestTheCacheSizeComes' -v
```
Expected: `TestATtlClampIsNotRendered` FAIL（`tagForeignTTL` 未定义）；另三个因为还没有渲染而 FAIL。

- [ ] **Step 3: 渲染 `foreign_ttl`**

`internal/mosdnsconfig/render.go`：

```go
	tagForeignTTL    = "foreign_ttl"
```

```go
	// ttlArgs clamps the TTL on every record in an answer. **BOTH keys are
	// omitempty and that is load-bearing**: mosdns's plugin reads `t.max > 0`
	// before applying a maximum (plugin/executable/ttl/ttl.go:91), and
	// ApplyMaximumTTL(m, 0) sets every record to TTL 0
	// (pkg/dnsutils/msg.go:65-67 -> :94-112). A zero bound must therefore be an ABSENT key,
	// not a written zero.
	ttlArgs struct {
		Max uint32 `yaml:"max,omitempty"`
		Min uint32 `yaml:"min,omitempty"`
	}
```

在 `renderable()` 的 `tagForeignForward` 之后：

```go
			// The TTL clamp, and ITS POSITION IS THE WHOLE OF IT.
			//
			// The plugin table order IS the next chain, and the cache stores
			// whatever its next returned (mosdns plugin/executable/cache/cache.go:209-211).
			// So a clamp BEFORE the forward would shape only the answer on the way
			// out while the router's own cache still expired on the upstream's TTL --
			// which is not what "the cache record time" means to an operator. After
			// the forward, the chain is cache -> forward -> clamp, so the cache stores
			// the clamped value and a hit returns the clamped value.
			//
			// Rendered only when the policy asks for a clamp, so an unchanged policy
			// leaves the committed document byte-identical.
			//
			// The CONDITIONAL, and not an unconditional entry with zero bounds: a zero
			// bound is a written `max: 0`, which mosdns applies as "set every record to
			// TTL 0". Omitting the whole plugin is what makes a zero mean "no clamp".
			if policy.ForeignCache.TTLMax > 0 || policy.ForeignCache.TTLMin > 0 {
				plugins = append(plugins, pluginArgs{
					Tag:  tagForeignTTL,
					Type: "ttl",
					Args: ttlArgs{Max: uint32(policy.ForeignCache.TTLMax), Min: uint32(policy.ForeignCache.TTLMin)},
				})
			}
```

**实现者注意**：`renderable()` 今天返回一个 `[]pluginArgs` 字面量。加这个条件条目时把它改成先构造 `plugins` 切片、再在需要时 append，最后返回——否则条件条目无法插进字面量里。**顺序仍然是 forward 之后**，所以 append 必须发生在 `tagForeignForward` 之后而不是函数末尾。

- [ ] **Step 4: 运行，确认它绿**

```
go test ./internal/mosdnsconfig/ -run 'TestATtlClamp|TestAZeroBound|TestTheCacheSizeComes' -v
```
Expected: PASS。

- [ ] **Step 5: 重新生成 `configs/mosdns.yaml`，跑整个包**

```
MOSDNS_ROUTER_UPDATE=1 go test ./internal/mosdnsconfig/ -run TestCommittedMosdnsConfig -v
go test ./internal/mosdnsconfig/ -v
```
Expected: 全 PASS；且 `git diff configs/mosdns.yaml` 只多出 `api:` 块和两个 upstream。

- [ ] **Step 6: 写失败的测试——`flush-cache`**

`cmd/mosdns-cdnctl/flush_cache_test.go`：

```go
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The path is the cache plugin's OWN endpoint, not one this project invented:
// coremain/mosdns.go:164-165 mounts a plugin's mux at /plugins/<tag> and
// plugin/executable/cache/cache.go:320-324 registers GET /flush on it. Getting
// this wrong produces a 404 that looks exactly like "the router has no cache".
func TestTheFlushPathIsTheCachePluginsOwnMountPoint(t *testing.T) {
	want := "/plugins/foreign_cache/flush"
	if flushCachePath() != want {
		t.Fatalf("flushCachePath() = %q, want %q", flushCachePath(), want)
	}
}

func TestFlushAsksTheRouterOverLoopbackAndReportsWhatItSaid(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if r.Method != http.MethodGet {
			t.Errorf("the flush was a %s, want GET", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	services := productionServices()
	services.flushBaseURL = server.URL
	var stdout, stderr bytes.Buffer
	if code := runFlushCache(nil, &stdout, &stderr, services); code != exitSuccess {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if asked != flushCachePath() {
		t.Fatalf("the router was asked for %q, want %q", asked, flushCachePath())
	}
	if !strings.Contains(stdout.String(), "flushed") {
		t.Fatalf("nothing said what happened:\n%s", stdout.String())
	}
}

func TestAFlushTheRouterRefusesIsAnErrorWithTheRoutersOwnWords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the router is not listening on its api address", http.StatusNotFound)
	}))
	defer server.Close()
	services := productionServices()
	services.flushBaseURL = server.URL
	var stdout, stderr bytes.Buffer
	if code := runFlushCache(nil, &stdout, &stderr, services); code != exitStateUnavailable {
		t.Fatalf("exit %d, want %d", code, exitStateUnavailable)
	}
	if !strings.Contains(stderr.String(), "not listening") {
		t.Fatalf("the router's own words were dropped, so an operator cannot tell a "+
			"refusal from a router that is down: %s", stderr.String())
	}
}
```

- [ ] **Step 7: 运行，确认它红**

```
go test ./cmd/mosdns-cdnctl/ -run 'TestTheFlushPath|TestFlushAsks|TestAFlushTheRouterRefuses' -v
```
Expected: 编译失败，`undefined: flushCachePath`。

- [ ] **Step 8: 写 `flush_cache.go` 与注入点**

`services` 加：

```go
	// flushBaseURL is the router's own API address. It is a field so the flush can
	// be pointed at an httptest server; production derives it from the routing
	// document's api.http, which is what makes the two impossible to disagree.
	flushBaseURL string
```

`productionServices()` 里：

```go
		flushBaseURL: "http://" + mosdnsconfig.APIListenAddress(),
```

`internal/mosdnsconfig` 导出：

```go
// APIListenAddress is the loopback address the routing document's api block binds.
// It is exported so the flush verb asks the address the document actually writes
// rather than a second copy of it.
func APIListenAddress() string { return apiListenAddress }
```

`cmd/mosdns-cdnctl/flush_cache.go`：

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// flushCachePluginTag is the tag the routing document gives the foreign cache, and
// it is part of the mounted path: coremain/mosdns.go:164-165 mounts a plugin's own
// mux under /plugins/<tag>.
const flushCachePluginTag = "foreign_cache"

// flushTimeout bounds the request. It is short because the endpoint is on
// loopback and answers or does not immediately; a slow answer means the router is
// not the thing this verb thinks it is.
const flushTimeout = 10 * time.Second

func flushCachePath() string { return "/plugins/" + foreignCachePluginTag + "/flush" }

// runFlushCache empties the router's foreign answer cache.
//
// **It is the router's OWN endpoint, not a mechanism this project built.**
// plugin/executable/cache/cache.go:320-324 registers GET /flush and :108 mounts it
// unconditionally; it has been unreachable only because the renderer emitted no
// api.http block (Task 2 adds one). So this verb is a thin, honest wrapper: there is
// no second flush path that could disagree with the router's.
//
// It takes no lock and writes nothing. Flushing a cache is not a state change
// anything else has to know about, and taking the control lock would make an
// operator's "clear the cache" refuse whenever a measurement was publishing.
func runFlushCache(args []string, stdout, stderr io.Writer, services services) int {
	if len(args) != 0 {
		writeCLIError(stderr, "flush-cache: takes no arguments, got %q", strings.Join(args, " "))
		return exitInvalidCLI
	}
	base := strings.TrimRight(services.flushBaseURL, "/")
	request, err := http.NewRequest(http.MethodGet, base+flushCachePath(), nil)
	if err != nil {
		writeCLIError(stderr, "flush-cache: %v", err)
		return exitStateUnavailable
	}
	client := &http.Client{Timeout: flushTimeout}
	response, err := client.Do(request)
	if err != nil {
		writeCLIError(stderr, "flush-cache: the router did not answer on %s: %v", base, err)
		return exitStateUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		// The router's own words, verbatim. A 404 here means the router is not
		// listening on its api address, which is a different fault from "the router
		// is down", and an operator needs to be able to tell them apart.
		writeCLIError(stderr, "flush-cache: the router answered %s: %s",
			response.Status, strings.TrimSpace(string(body)))
		return exitStateUnavailable
	}
	writeReportLine(stdout, "flushed: %s%s\n", base, flushCachePath())
	return exitSuccess
}
```

`main.go` dispatch 加：

```go
	case "flush-cache":
		return runFlushCache(args[1:], stdout, stderr, services)
```

- [ ] **Step 9: 运行，确认它绿**

```
go test ./cmd/mosdns-cdnctl/ -run 'TestTheFlushPath|TestFlushAsks|TestAFlushTheRouterRefuses' -v
```
Expected: PASS。

- [ ] **Step 10: 加一个真的起 mosdns 实例的集成用例**

`tests/integration/` 新增 `flush_cache_test.go`（或加进现有文件）：渲染文档、起一个 mosdns 实例、查一次 foreign 域名（进缓存）、再查一次并数上游收到的查询数、跑 flush、再查一次并确认上游**又**收到了一次。

**这一条是 flush 唯一有意义的断言**：「endpoint 返回 200」不证明缓存被清了，起一个真实例前后对比才证明。中间三步（查 → flush → 再查）必须数上游查询数，不能只看回答是否相同——相同回答无法区分命中与未命中。

- [ ] **Step 11: 变异验证**

```bash
# Task 4 渲染部分
cp internal/mosdnsconfig/render.go /home/ubuntu/.gotmp-mosdns/render4.bak
# a. ttl 插件放在 forward 之前
python3 - <<'PY'
from pathlib import Path
p=Path('internal/mosdnsconfig/render.go'); t=p.read_text()
i=t.index('Tag:  tagForeignTTL'); j=t.index('Tag:  tagForeignForward')
t=t[:i]+t[j:]
p.write_text(t)
PY
go test ./internal/mosdnsconfig/ -run TestATtlClampIsRenderedAfter 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render4.bak internal/mosdnsconfig/render.go
# b. 零值写成 min: 0
python3 - <<'PY'
from pathlib import Path
p=Path('internal/mosdnsconfig/render.go'); t=p.read_text()
t=t.replace('Min uint32 `yaml:"min,omitempty"`','Min uint32 `yaml:"min"`')
p.write_text(t)
PY
go test ./internal/mosdnsconfig/ -run TestAZeroBoundIsOmitted 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render4.bak internal/mosdnsconfig/render.go
# c. flush 路径写错
cp cmd/mosdns-cdnctl/flush_cache.go /home/ubuntu/.gotmp-mosdns/fc.bak
sed -i 's|return "/plugins/" + foreignCachePluginTag + "/flush"|return "/flush"|' cmd/mosdns-cdnctl/flush_cache.go
go test ./cmd/mosdns-cdnctl/ -run 'TestTheFlushPath|TestFlushAsks' 2>&1 | grep -c FAIL  # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/fc.bak cmd/mosdns-cdnctl/flush_cache.go
go test ./internal/mosdnsconfig/ ./cmd/mosdns-cdnctl/ 2>&1 | tail -3   # 期望 ok
```

- [ ] **Step 12: 提交**

```bash
git add internal/mosdnsconfig/ cmd/mosdns-cdnctl/ configs/mosdns.yaml tests/integration/
git commit -m "feat: a cache record time an operator can set, and a way to clear it

lazy_cache_ttl is NOT a TTL. At mosdns plugin/executable/cache/cache.go:198 it is
used as a BOOLEAN for stale-while-revalidate: an expired entry is returned with a
five-second TTL while a refresh runs behind it. So record time is controlled with
mosdns's own ttl plugin instead, and the placement is the whole of it.

The clamp goes AFTER foreign_forward in the plugin table, because the table order
IS the next chain and the cache stores whatever its next returned (cache.go:209-211).
Before the forward, the clamp would shape only the answer on the way out while the
router's own cache still expired on the upstream's TTL -- which is not what an
operator asking for a record time means.

Both ttl keys are omitempty and that is load-bearing: mosdns reads `t.max > 0`
before applying a maximum (ttl.go:91), and ApplyMaximumTTL(m, 0) sets every record
to TTL 0 (pkg/dnsutils/msg.go:65-67 -> :94-112). A zero bound must be an ABSENT key. It is
also what keeps the committed routing document byte-identical when the policy asks
for no clamp.

flush-cache is not a new mechanism. cache.go:320-324 registers GET /flush, :108
mounts it unconditionally, and it has been unreachable only because the renderer
emitted no api.http block. The verb is a thin wrapper over the router's own
endpoint, deliberately: a second flush path could disagree with the router's. It
takes no lock, because an operator clearing a cache should not be refused while a
measurement is publishing.

The integration case starts a real mosdns instance and COUNTS the queries the
upstream receives across a flush, because two identical answers cannot tell a hit
from a miss.

Mutation-verified: moving the clamp before the forward, writing min: 0, and
pointing the flush at the wrong path each turn the matching case red."
```

---

### Task 5: 既有测试的改写

**Files:**
- Modify: `installer/tests/test_units.py:1363-1375`
- Modify: `tests/integration/routing_test.go:1296-1320`、`:1230-1284`
- Modify: `cmd/mosdns-cdnctl/render_test.go:137-175`（若需要重生成）
- Test: 无新文件（这一任务是改写）

**Interfaces:**
- Consumes: Task 1-4 的全部产物
- Produces: 无新接口

- [ ] **Step 1: 跑全门禁，列出所有失败**

```
MOSDNS_ROUTER_UPDATE=1 go test ./... 2>&1 | grep -v '^ok \|no test files'
cd installer/tests && python3 -m unittest discover -s . -p 'test_*.py' 2>&1 | grep -E '^FAIL:|^ERROR:'
```

把失败清单写下来。**每一条都要判断「新形状下它守的性质是否还成立」**，然后改写。不成立的删掉并在本 commit message 里说明理由；成立的改成守新性质。

- [ ] **Step 2: 改写 `installer/tests/test_units.py` 的上游断言**

`test_the_routing_document_forwards_to_the_loopback_resolver` 今天对 `upstreams` 列表做等值断言 `["tcp://127.0.0.1:15353"]`。改成：**列表等于策略里启用的 `upstream` 条目的 `addr`，按策略顺序**——并且它必须从**已发布的 policy.yaml** 读期望值，不是从常量：

```python
    def test_the_routing_document_forwards_to_exactly_the_enabled_upstreams(self):
        """The routing document's forward IS the policy's enabled upstream entries.

        A literal list in this test is a second copy of the policy that can drift
        from it, and a test that passes against a document naming a different route
        than the operator configured is worse than no test. So the expectation is
        READ from the shipped policy, which is the document the operator edits.
        """
        policy = self.policy_document()
        enabled = [
            entry["addr"] for entry in policy["foreign"]["upstreams"]
            if entry.get("enabled", True) and entry["kind"] == "upstream"
        ]
        rendered = self.foreign_upstream_addrs()
        self.assertEqual(rendered, enabled, ...)
```

- [ ] **Step 3: 改写 `tests/integration/routing_test.go` 的「恰好一次」断言**

`TestAForeignQueryReachesTheResolverOnlyOverTCP`（`:1296`）断言恰好 1 次 TCP、0 次 UDP。`concurrent: 2` 下会问两条。改成：

- **保留**「UDP 一次都没有」这一条——它守的是 `foreign_upstream` 是 tcp 而不是 udp，这是 §2.1 的实测结论，值得单独守。
- 把「恰好 1 次 TCP」换成「两条上游都被问过，且答案来自先应答的那个」。
- **新增**「一条上游不可达时查询仍然成功」（Review Focus #5）。

- [ ] **Step 4: 跑 Go 与 Python 门禁**

```
MOSDNS_ROUTER_UPDATE=1 go test ./... 2>&1 | grep -v '^ok \|no test files'   # 期望空
(cd installer/tests && python3 -m unittest discover -s . -p 'test_*.py' 2>&1 | tail -4)
(cd bridge && python3 -m unittest discover -s tests -p 'test_*.py' 2>&1 | tail -3)
```

`installer/tests` 允许的唯一失败仍是 `test_the_man_stub_is_what_keeps_documentation_from_deciding_the_verdict`（先前的、断言 systemd 249/255 而本机是 259）。Podman 单元是下一个 task 的事，不在这里跑。

- [ ] **Step 5: 变异验证——证明改写后的断言还咬得动**

这一 task 改的是断言，所以变异验证的方向是**反的**：把实现改坏，确认**改写后的**断言变红。改写一个断言最常见的失败方式不是它变红，而是它变得什么都抓不住，而那只有改坏实现才看得出来。

```bash
cp internal/mosdnsconfig/render.go /home/ubuntu/.gotmp-mosdns/render5.bak

# a. 把渲染器退回改动前的样子（永远只渲染 dnscrypt listener）-> test_units.py 的
#    「等于策略里启用的条目」必须红
python3 - <<'PY'
from pathlib import Path
p = Path('internal/mosdnsconfig/render.go'); t = p.read_text()
old = 'Upstreams:  resolved.forwardUpstreams,'
assert t.count(old) == 1, f"替换点命中 {t.count(old)} 次，改代码后先看清楚再改这里"
t = t.replace(old, 'Upstreams:  []upstreamArgs{{Addr: resolved.ForeignListener}},')
p.write_text(t)
PY
(cd installer/tests && python3 -m unittest test_units 2>&1 | grep -c 'FAIL')   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render5.bak internal/mosdnsconfig/render.go

# b. ECH 来源改成 udp -> 集成测试的「UDP 一次都没有」必须红
python3 - <<'PY'
from pathlib import Path
p = Path('internal/mosdnsconfig/render.go'); t = p.read_text()
t = t.replace('ForeignUpstream: resolved.echUpstream',
              'ForeignUpstream: "udp://" + strings.TrimPrefix(resolved.echUpstream, "tcp://")')
p.write_text(t)
PY
go test ./tests/integration/ -run TestAForeignQueryReaches 2>&1 | grep -c 'FAIL'   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render5.bak internal/mosdnsconfig/render.go

# c. concurrent 写死成 1 -> 改写后的「两条上游都被问过」必须红
python3 - <<'PY'
from pathlib import Path
p = Path('internal/mosdnsconfig/render.go'); t = p.read_text()
t = t.replace('Concurrent: policy.Foreign.Concurrent,', 'Concurrent: 1,')
p.write_text(t)
PY
go test ./tests/integration/ -run TestAForeignQueryReaches 2>&1 | grep -c 'FAIL'   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/render5.bak internal/mosdnsconfig/render.go
go test ./tests/integration/ 2>&1 | tail -2   # 期望 ok
```

**每个变异脚本里的 `t.count(old) == 1` 断言不是形式。** 上一版这条变异写的是把上游列表截成 `[:1]`——而默认策略里恰好只有一条启用的 `upstream` 条目，所以那是个**空操作**：替换成功、编译成功、测试照样全绿，这一步会**假装通过**。凡是「改坏实现看测试红不红」的步骤，替换点命中数不是 1 就当场失败，不要继续。

**用 `cp` 恢复，不要用 `git checkout`**：本 worktree 的 `.gocache-review/` 是未跟踪的 753MB，`git checkout .` 之类会把它一起算进去（这正是本 worktree 禁用 `git stash -u` 的同一个理由）。

**如果 (a) 不红**，说明 `test_the_routing_document_forwards_to_exactly_the_enabled_upstreams` 写歪了——它八成又在比较一个常量列表而不是从 `policy.yaml` 读期望值。**如果 (b) 不红**，说明「UDP 一次都没有」那一半在改写中被顺手删掉了，而它守的是 §2.1 的实测结论。**三条都红，这个 task 才算做完。**

- [ ] **Step 6: 提交**

```bash
git add internal/ tests/integration/ cmd/ installer/tests/
git commit -m "test: the assertions this change breaks, rewritten to hold the new property

Every one of these was a shape assertion that a changed shape made false, and
turning a false shape assertion into a deleted test is how this project has
repeatedly lost a real gate. Each is rewritten to hold what it was actually for.

test_the_routing_document_forwards_to_the_loopback_resolver asserted a literal
upstream list. That literal was a second copy of the policy, so it could drift
from it and pass against a document naming a different route than the operator
configured. It now READS the expectation from the shipped policy.yaml.

TestAForeignQueryReachesTheResolverOnlyOverTCP asserted exactly one TCP query and
zero UDP. The zero-UDP half is kept on its own, because it holds the measured fact
that the ECH path must not use udp. The exactly-one half is replaced: at
concurrent 2 two upstreams are asked, and what matters is that the answer came from
whichever answered first.

Mutation-verified here in the reverse direction, because this commit changes
assertions and a rewritten assertion's typical failure is not going red but going
blind: dropping a rendered upstream turns test_units red, writing the ECH source as
udp:// turns the zero-UDP half red, and pinning concurrent to 1 turns the
both-upstreams-asked half red.

The Podman cells gain their case in the next task; nothing here depends on them."
```

---

### Task 6: Podman 单元

**Files:**
- Modify: `tests/podman/scenarios/routing_test.py`
- Modify: `tests/podman/tests/test_install_routing_scenario.py`（若断言需要跟着改）
- Test: 无新文件（这一任务是加断言）

**Interfaces:**
- Consumes: Task 2 的渲染、Task 5 改写后的集成用例
- Produces: 无新接口

**为什么单独一个 task**：spec §5.4 把它标为「这个规范里最大的一块测试工作量」，而且它是唯一一个 reviewer 可以批准 Task 5 同时拒绝本 task 的东西——Go 与 Python 门禁不依赖它，Podman 门禁依赖它。

- [ ] **Step 1: 确认单元里 DoQ 条目确实是死的，然后证明这不影响结果**

Podman 单元按设计没有到外网的路由，所以 `configs/policy.yaml` 里那条 `quic://dns.quad9.net:853` 在单元里必然不可达。**这不是要修的东西**（spec §8.2）：`concurrent: 2` 下一次查询在第一个非错误答案到达时就返回，每次尝试拿自己独立的 5 秒上下文（mosdns `forward.go:272`），而收集循环在第一个非错误答案就返回（`:302-319`），所以死掉的上游不增加延迟。

先把这个前提测出来，而不是假设它：

```python
    def test_the_doq_upstream_is_unreachable_in_this_cell_and_the_cell_still_installs(self):
        """The precondition, measured rather than assumed.

        The shipped policy names a DoQ Quad9 upstream and this cell has no route off
        its own bridge, so that entry CANNOT answer here. The install must still
        succeed, and the reason it does is `concurrent: 2` plus the DNSCrypt entry
        that foreign_override.py points at a mock.

        A cell that silently gained a route would make this case pass for the wrong
        reason, so it asserts the failure it depends on: the DoQ entry does not
        answer from inside the cell.
        """
        # in the target container, with no route off the bridge:
        #   dig +time=3 +tries=1 @<the DoQ address> A example.com   -> fails
        # and then the package is still `install ok installed`.
```

- [ ] **Step 2: 加「一条上游不可达时查询仍成功」的断言**

`tests/podman/scenarios/routing_test.py` 新增场景：

1. 第一个上游（DNSCrypt，被 `foreign_override.py` 指向 mock）可达 ⇒ 查询成功
2. 第二个上游（DoQ）不可达 ⇒ 查询**仍然成功**
3. **断言不可达的那条上游确实被问过**：数它的查询计数

**第 3 条是这个场景的全部意义。** 只断言「查询成功」的话，一个 `concurrent: 1` 的文档也会通过，而那正是这个改动要改掉的东西。这个场景是 spec §5.4 里唯一被点名要新增的断言。

- [ ] **Step 3: 加一个 `concurrent: 1` 的对照单元**

同一个场景，把 policy 的 `concurrent` 改成 1，断言**查询仍然成功**（因为 1 是随机单选，而 DNSCrypt 那条在这一轮被选中了）——但**不可达的那条上游这次没有被问过**。

两个场景合起来才把 `concurrent` 的语义钉死：`2` 会问两条、取先应答的；`1` 只问一条、随机挑。没有第 3 条时前一个场景分不清 `2` 和 `1`。

- [ ] **Step 4: 跑 Podman 门禁**

```
python3 -m pytest tests/podman/ -x -q 2>&1 | tail -20
```
（若 pytest 不可用，按 `tests/podman/run.py` 的用法跑；harness 的调用方式以 `docs/testing.md` 为准。）

Expected: 全绿。

- [ ] **Step 5: 变异验证**

把渲染器的 `Concurrent` 写死成 1，重跑 Step 2 的场景，**它必须变红**（因为不可达的那条上游不再被问过）。把 `concurrent` 写死成 3 也一样要红——两条上游而 `concurrent: 3`，`forward.go:266` 的 `us[(r+i)%len(us)]` 会绕回第一条，于是第二条被问两次、第一条一次，计数对不上。

两次变异都必须记录在 commit message 里。

- [ ] **Step 6: 提交**

```bash
git add tests/podman/
git commit -m "test(podman): one upstream unreachable and the query still succeeds

This is the assertion the spec named as the one thing a cell must gain, and it is
the only thing that justifies concurrent: 2. Without it, concurrent is a number
nobody verified.

It COUNTS the queries the unreachable upstream received, and that count is the
whole case. Asserting only 'the query succeeded' passes against a concurrent: 1
document too -- which is exactly the default this change moves away from.

A companion cell sets concurrent: 1 and asserts the unreachable upstream is NOT
asked. Together the two pin the semantics: 2 asks two and takes the first answer,
1 asks one and picks at random.

The cell also measures its own precondition rather than assuming it: the shipped
DoQ Quad9 entry cannot answer in a cell with no route off its bridge, and the
install still succeeds. A cell that had quietly gained a route would make the case
pass for the wrong reason.

Mutation-verified: pinning Concurrent to 1 turns the counting assertion red, and
pinning it to 3 turns it red the other way -- with two upstreams, forward.go:266
wraps to the first one, so the second is asked twice."
```

---

### Task 7: 文档

**Files:**
- Modify: `packaging/man/mosdns-cdnctl.1`（SYNOPSIS + 新章节）
- Modify: `packaging/man/mosdns-router.8`（FILES 里 `policy.yaml` 的形状）
- Modify: `installer/tests/test_package.py`（加门禁）

**Interfaces:**
- Consumes: 全部前序任务
- Produces: 无新接口

- [ ] **Step 1: 写失败的测试——man page 覆盖新表面**

`installer/tests/test_package.py` 追加（放在上一个 commit 加的那批文档门禁旁边）：

```python
    def test_the_manual_documents_the_configurable_foreign_route(self):
        """Four things an operator cannot guess, and every one of them is a
        decision rather than an implementation detail.

          * **concurrent: 1 is a random pick, not a race.** An operator who sets it
            to 1 and has four upstreams believes they have redundancy; they have
            load balancing. The manual has to say so in those words.
          * **bootstrap is not a query path.** A domain upstream with a bootstrap
            looks like the router forwards foreign queries to 9.9.9.9 in the clear.
            It does not, and an operator reading only the YAML cannot tell.
          * **disabling the dnscrypt entry does not stop the unit.** It stops the
            router using it. Nothing else changes, and an operator who expected
            dnscrypt-proxy to be gone will find it running.
          * **an upstream with no tcp:// route is refused.** The consequence --
            every forced-ECH name failing closed once the key's grace ends -- is the
            single most surprising thing this configuration can do.
        """
        manual = shipped_prose(REPO / "packaging" / "man" / "mosdns-cdnctl.1")
        lowered = manual.lower()
        for phrase, because in (
            ("random", "concurrent 1 is a random pick and an operator with four "
                       "upstreams believes they have redundancy"),
            ("bootstrap", "a bootstrap looks like the router forwards foreign queries "
                          "in the clear, and it does not"),
            ("does not stop", "an operator who disables the dnscrypt entry expects the "
                              "unit to be gone"),
            ("ech", "a route with no tcp:// entry is refused and the consequence is "
                    "every forced-ECH name failing closed"),
        ):
            with self.subTest(phrase=phrase):
                self.assertIn(phrase, lowered,
                              f"mosdns-cdnctl(1) does not mention {phrase!r}, so an operator "
                              f"meets {because} with nothing written down")

    def test_both_new_verbs_are_in_the_synopsis(self):
        """The gate the previous commit added, extended: a verb that exists and is
        not in the SYNOPSIS is a verb nobody can find, and `apply` and `status` were
        both shipped that way."""
        raw = (REPO / "packaging" / "man" / "mosdns-cdnctl.1").read_text(encoding="utf-8")
        section = re.search(r"^\.SH SYNOPSIS$(.*?)^\.SH DESCRIPTION$", raw, re.MULTILINE | re.DOTALL)
        self.assertIsNotNone(section, "mosdns-cdnctl(1) has no SYNOPSIS section")
        prologue = r"(?:\.[A-Za-z]+(?:\([^)]*\))?\s*)*"
        for verb in ("check-upstream", "flush-cache"):
            with self.subTest(verb=verb):
                escaped = r"\\?-".join(re.escape(p) for p in verb.split("-"))
                self.assertRegex(section.group(1), rf"(?m)^\s*{prologue}{escaped}\b",
                                 f"{verb} is implemented and the SYNOPSIS does not list it")
```

- [ ] **Step 2: 运行，确认它红**

```
cd installer/tests && python3 -m unittest test_package.PinnedRangeSnapshotTests -v
```
Expected: FAIL（man page 里没有这些词，`check-upstream` 不在 SYNOPSIS）。

- [ ] **Step 3: 写 SYNOPSIS 的两个新 verb**

在 `mosdns-cdnctl.1` 的 SYNOPSIS 里，`emergency-rollback` 之前插入：

```roff
.B mosdns\-cdnctl
.I check\-upstream
.RB ( \-\-all | \fINAME\fR )
.RB [ \-\-policy\ \fIPATH\fR ]
.br
.B mosdns\-cdnctl
.I flush\-cache
.br
```

- [ ] **Step 4: 写「境外上游」章节**

在 `update-lists` 章节之后、`test` 章节之前插入一节 `.SS "The foreign route"`。必须覆盖 Step 1 那四条，并且写出：

- 条目的四个字段与它们的含义（`kind`/`name`/`enabled`/`addr`/`bootstrap`）
- **`concurrent` 的准确含义**：`1` 是随机单选，**没有故障转移**；`2`（默认）是一次查询问两个、取先应答的；代价是每次查询多一次出站尝试和每次失败一行 `Warn`，**不是延迟**
- **`bootstrap` 不是查询路径**：它只用来解析该条目自己的域名，因为本机唯一的解析器就是路由器本身
- **DNSCrypt stamp 仍在 conffile**：新增 provider 要手改 `/etc/mosdns/dnscrypt-proxy.toml` 并重启 `dnscrypt-proxy`。引用现有的 `packaging/debian/conffiles:44-45` 那条设计说明
- **禁用 `dnscrypt` 条目不会停掉 `dnscrypt-proxy` unit**：它只是让路由器不再往那儿发查询；停不停是操作员的事
- **没有 `tcp://` 路由会被拒绝**，以及后果
- 默认值是什么（DNSCrypt Quad9 + DoQ Quad9，`concurrent: 2`）

- [ ] **Step 5: 写两个新 verb 的 DESCRIPTION**

`check-upstream`：它做什么、探测名为什么是 `example.com`、输出格式、退出码（0 全部可达 / 3 有不可达）、**dnscrypt 条目查的就是 ECH 走的那条路所以这一行也回答「还能不能取到 ECH key」**、**一个可达但拒绝的 resolver 与一个不可达的不是一回事**。

`flush-cache`：它用的是路由器自己的端点（引用 `cache.go:320-324`），不是本项目造的第二条路径；不取锁；router 拒答时把**它自己的话**原样带出来，因为 404（路由器没在 api 地址上听）和「路由器没起来」是两个故障。

- [ ] **Step 6: 更新 `mosdns-router.8` 的 FILES**

`policy.yaml` 那一条现在要描述新形状（`foreign.upstreams` 列表 + `foreign_cache` 块），并指向 `mosdns-cdnctl(1)`。

- [ ] **Step 7: 运行，确认它绿**

```
cd installer/tests && python3 -m unittest discover -s . -p 'test_*.py' 2>&1 | tail -4
groff -man -Tascii -ww packaging/man/mosdns-cdnctl.1 >/dev/null && echo "man OK"
groff -man -Tascii -ww packaging/man/mosdns-router.8 >/dev/null && echo "man OK"
```

Expected: `test_the_manual_documents_the_configurable_foreign_route` 与 `test_both_new_verbs_are_in_the_synopsis` PASS；installer 只有那一个先前的 systemd 失败。

- [ ] **Step 8: 变异验证——文档门禁必须真的会红**

一个只会 PASS 的文档门禁比没有门禁更糟：它让人以为文档被看着。所以把 man page 改坏，确认门禁抓住。**四个方向各改一处**，因为四条里任何一条写成永真断言都不该被放过。

```bash
cp packaging/man/mosdns-cdnctl.1 /home/ubuntu/.gotmp-mosdns/cdnctl.1.bak

# a. 删掉 concurrent 的真实语义那一段 -> "random" 门禁必须红
python3 - <<'PY'
import re
from pathlib import Path
p = Path('packaging/man/mosdns-cdnctl.1'); t = p.read_text()
# 把所有 RANDOM/random 换成 PICK，语义消失但词还在别的行里的话下面第二道
t = re.sub(r'(?i)randomly|random pick|RANDOM', 'opaque selection', t)
p.write_text(t)
PY
(cd installer/tests && python3 -m unittest test_package 2>&1 | grep -c 'FAIL')   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cdnctl.1.bak packaging/man/mosdns-cdnctl.1

# b. 把 SYNOPSIS 里的两个新 verb 删掉 -> synopsis 门禁必须红
python3 - <<'PY'
import re
from pathlib import Path
p = Path('packaging/man/mosdns-cdnctl.1'); t = p.read_text()
head, sep, rest = t.partition('.SH DESCRIPTION')
head = re.sub(r'[\\]?-[ ]*I(?:check|flush)[\\]?-', '', head)
p.write_text(head + sep + rest)
PY
(cd installer/tests && python3 -m unittest test_package 2>&1 | grep -c 'FAIL')   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cdnctl.1.bak packaging/man/mosdns-cdnctl.1

# c. 删掉「禁用 dnscrypt 不停 unit」那句 -> "does not stop" 门禁必须红
python3 - <<'PY'
from pathlib import Path
p = Path('packaging/man/mosdns-cdnctl.1'); t = p.read_text()
t = t.replace('does not stop', 'stops')
p.write_text(t)
PY
(cd installer/tests && python3 -m unittest test_package 2>&1 | grep -c 'FAIL')   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cdnctl.1.bak packaging/man/mosdns-cdnctl.1

# d. 删掉整个境外上游章节 -> 上面四道里至少三道同时红
python3 - <<'PY'
import re
from pathlib import Path
p = Path('packaging/man/mosdns-cdnctl.1'); t = p.read_text()
t = re.sub(r'(?m)^\.SS "The foreign route".*?(?=^\.SS |^\.SH )', '', t, flags=re.S)
p.write_text(t)
PY
(cd installer/tests && python3 -m unittest test_package 2>&1 | grep -c 'FAIL')   # 期望 >=1
cp /home/ubuntu/.gotmp-mosdns/cdnctl.1.bak packaging/man/mosdns-cdnctl.1

(cd installer/tests && python3 -m unittest test_package 2>&1 | tail -3)   # 期望回到只有那一个 systemd 失败
```

**(d) 是最重要的一条。** (a)-(c) 各证明一道门禁会红；(d) 证明整套门禁真的绑在那一节上，而不是碰巧在别处匹配到了那些词。**如果 (d) 只红一道，说明另外三道里有两道是从整份 man page 里随便一处匹配到关键词的，而不是从那一节读到的**——那就要把断言收紧到只搜那一节。

- [ ] **Step 9: 提交**

```bash
git add packaging/man/ installer/tests/test_package.py
git commit -m "docs: the foreign route, and the two verbs that act on it

Four things an operator cannot guess, each held by a gate added in the same change
rather than asserted here in prose:

  * concurrent: 1 is a RANDOM PICK (mosdns forward.go:265-267), so an operator who
    sets it to 1 and has four upstreams believes they have redundancy and has load
    balancing. The manual says so in those words, and says the cost of 2 is one
    extra outbound attempt and a warn line per failure, not latency.
  * bootstrap resolves the entry's own host name and is NOT a query path. A domain
    upstream with a bootstrap looks, read from YAML alone, like the router forwards
    foreign queries to 9.9.9.9 in the clear.
  * Disabling the dnscrypt entry does NOT stop dnscrypt-proxy. It stops the router
    using it, the unit keeps running, and the installer's readiness barrier still
    applies. An operator who expected the unit to be gone will find it running.
  * A route with no tcp:// entry is refused by name, because the consequence is
    every forced-ECH name failing closed once the key's grace ends.

DNSCrypt stamps staying in the conffile is documented where an operator adding a
provider will look, rather than left to the design note in packaging/debian/
conffiles that only this repository reads.

check-upstream's probe name and exit codes, and flush-cache's use of the router's
own endpoint, are documented because both are decisions a reader would otherwise
have to take on trust.

Mutation-verified, because a documentation gate that can only pass is worse than
none: it lets a reader believe the documentation is watched. Removing the
concurrent semantics, removing both verbs from the SYNOPSIS, and inverting the
"disabling does not stop the unit" sentence each turn the matching gate red. The
fourth check deletes the whole section, and it must turn at least three of them
red -- if it turns only one, two of the four gates were matching a keyword
somewhere else in the manual and have to be narrowed to the section."
```

---

## 附：整条计划的完成判据

全部任务做完后，下面这一串必须全绿：

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=/home/ubuntu/project_v2/.worktrees/mosdns-router/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns
export TMPDIR=/home/ubuntu/.gotmp-mosdns
cd /home/ubuntu/project_v2/.worktrees/mosdns-router

gofmt -l . | grep -v '^\.gocache-review'          # 空
go vet ./...                                       # 空
MOSDNS_ROUTER_UPDATE=1 go test ./... 2>&1 | grep -v '^ok \|no test files'   # 空
(cd installer/tests && python3 -m unittest discover -s . -p 'test_*.py')   # 仅 1 个先前失败
(cd bridge && python3 -m unittest discover -s tests -p 'test_*.py')         # ok
sh scripts/build-deb.sh                             # 成功
```

**并且 `git diff` 在重新生成 `configs/policy.yaml` 与 `configs/mosdns.yaml` 之后为空**——这是「策略不变 ⇒ 文档不变」这条性质的端到端证明。
