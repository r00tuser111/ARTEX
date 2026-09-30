package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache 是保护 GitHub 配额的那一层：未认证的 API 只有 60 次/小时/IP，
// 而顶栏的"有新版本"提示每次整页加载都会查一次。缓存一旦失效，用户多开几个
// 标签页就会把配额耗光，之后真想更新反而查不动。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

// 非官方发布源在页面上必须藏不住：前端靠 repo_official 决定要不要标红并追加一次
// 确认，靠 repo_source 告诉运维这个值是从哪个开关来的。这几个字段错了，用户会在
// 不知情的情况下从别人的仓库装上一个会替换 artex 本体的二进制。
// 代理那几个字段同理——更新走哪条出网路径必须看得见，且绝不能回显凭据。

// restoreUpdateSource 在测试结束后还原 main 注入的那几个全局值。
func restoreUpdateSource(t *testing.T) {
	t.Helper()
	updSourceMu.Lock()
	from, cfgErr := updSourceFrom, updSourceErr
	proxyFrom, proxyErr := updProxyFrom, updProxyCfgErr
	updSourceMu.Unlock()
	t.Cleanup(func() {
		SetUpdateSource(from, cfgErr)
		SetUpdateProxySource(proxyFrom, proxyErr)
	})
}

// withUpdateProxy 设置更新专用代理并在测试后还原。
func withUpdateProxy(t *testing.T, raw string) {
	t.Helper()
	old := selfupdate.Proxy()
	t.Cleanup(func() {
		if err := selfupdate.SetProxy(old); err != nil {
			t.Errorf("还原更新代理: %v", err)
		}
	})
	if err := selfupdate.SetProxy(raw); err != nil {
		t.Fatalf("SetProxy(%q): %v", raw, err)
	}
}

func TestUpdateSourceInfoDefaultsToOfficial(t *testing.T) {
	restoreUpdateSource(t)
	SetUpdateSource("内置默认值", "")

	got := updateSourceInfo("")
	if got["repo"] != selfupdate.DefaultRepo {
		t.Errorf("repo = %v, want %s", got["repo"], selfupdate.DefaultRepo)
	}
	if got["repo_official"] != true {
		t.Errorf("repo_official = %v, want true", got["repo_official"])
	}
	// 没有配置错误时不该出现 repo_error，否则前端会凭空弹一条警告。
	if _, ok := got["repo_error"]; ok {
		t.Errorf("默认源不应带 repo_error，得到 %v", got["repo_error"])
	}
}

func TestUpdateSourceInfoReportsNonOfficialSource(t *testing.T) {
	restoreUpdateSource(t)
	old := selfupdate.Repo()
	t.Cleanup(func() {
		if err := selfupdate.SetRepo(old); err != nil {
			t.Errorf("还原发布源: %v", err)
		}
	})
	if err := selfupdate.SetRepo("someone/fork"); err != nil {
		t.Fatal(err)
	}
	SetUpdateSource("环境变量 ARTEX_UPDATE_REPO", "")

	got := updateSourceInfo("")
	if got["repo"] != "someone/fork" {
		t.Errorf("repo = %v, want someone/fork", got["repo"])
	}
	if got["repo_official"] != false {
		t.Errorf("repo_official = %v, want false", got["repo_official"])
	}
	if got["repo_source"] != "环境变量 ARTEX_UPDATE_REPO" {
		t.Errorf("repo_source = %v", got["repo_source"])
	}
}

// 配错了的情况：实际生效的是官方源，但要把那条错误一起报出去，否则运维会以为
// 自己改的发布源已经生效了。
func TestUpdateSourceInfoSurfacesConfigError(t *testing.T) {
	restoreUpdateSource(t)
	SetUpdateSource("环境变量 ARTEX_UPDATE_REPO", "发布源 \"nope\" 无效")

	got := updateSourceInfo("")
	if got["repo"] != selfupdate.DefaultRepo || got["repo_official"] != true {
		t.Errorf("配置无效时应退回官方源，得到 repo=%v official=%v", got["repo"], got["repo_official"])
	}
	if got["repo_error"] != "发布源 \"nope\" 无效" {
		t.Errorf("repo_error = %v", got["repo_error"])
	}
}

// 三种出网情形各自的说明必须能区分开。尤其是"没配专用代理但有全局代理"这一种：
// 更新链路一直跟着全局代理走，页面上说成直连会让用户排错时找错方向。
func TestUpdateSourceInfoReportsDirectConnection(t *testing.T) {
	restoreUpdateSource(t)
	withUpdateProxy(t, "")
	SetUpdateProxySource("", "")

	got := updateSourceInfo("")
	if got["proxy_set"] != false {
		t.Errorf("proxy_set = %v, want false", got["proxy_set"])
	}
	if got["proxy"] != "" {
		t.Errorf("直连时 proxy 应为空，得到 %v", got["proxy"])
	}
	if got["proxy_source"] != "未配置 · 直连 GitHub" {
		t.Errorf("proxy_source = %v", got["proxy_source"])
	}
}

func TestUpdateSourceInfoReportsGlobalProxyFallback(t *testing.T) {
	restoreUpdateSource(t)
	withUpdateProxy(t, "")
	SetUpdateProxySource("", "")

	got := updateSourceInfo("http://127.0.0.1:7890")
	if got["proxy_set"] != true {
		t.Errorf("proxy_set = %v, want true", got["proxy_set"])
	}
	if got["proxy"] != "http://127.0.0.1:7890" {
		t.Errorf("proxy = %v", got["proxy"])
	}
	if got["proxy_source"] != "全局出口代理 · 系统设置" {
		t.Errorf("proxy_source = %v", got["proxy_source"])
	}
}

func TestUpdateSourceInfoPrefersDedicatedProxy(t *testing.T) {
	restoreUpdateSource(t)
	withUpdateProxy(t, "socks5://127.0.0.1:1080")
	SetUpdateProxySource("环境变量 ARTEX_UPDATE_PROXY", "")

	// 专用代理必须盖过全局代理，否则"给更新单独开一条通道"这件事就没落地。
	got := updateSourceInfo("http://127.0.0.1:7890")
	if got["proxy"] != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %v, want socks5://127.0.0.1:1080", got["proxy"])
	}
	if got["proxy_source"] != "更新专用代理 · 来自环境变量 ARTEX_UPDATE_PROXY" {
		t.Errorf("proxy_source = %v", got["proxy_source"])
	}
}

// 这个接口对所有登录用户开放，回显的代理地址里绝不能带密码。
func TestUpdateSourceInfoRedactsProxyCredentials(t *testing.T) {
	restoreUpdateSource(t)
	withUpdateProxy(t, "socks5://alice:s3cret@127.0.0.1:1080")
	SetUpdateProxySource("配置文件 /etc/artex/config.json (update.proxy)", "")

	got := updateSourceInfo("")
	shown, _ := got["proxy"].(string)
	if strings.Contains(shown, "s3cret") {
		t.Fatalf("代理密码被回显了: %q", shown)
	}
	if !strings.Contains(shown, "alice") || !strings.Contains(shown, "127.0.0.1:1080") {
		t.Errorf("脱敏后仍应能认出用户名和主机，得到 %q", shown)
	}

	// 全局代理那条路同样要脱敏——它来自数据库，一样可能带凭据。
	withUpdateProxy(t, "")
	SetUpdateProxySource("", "")
	got = updateSourceInfo("http://bob:hunter2@proxy.local:8080")
	if shown, _ := got["proxy"].(string); strings.Contains(shown, "hunter2") {
		t.Fatalf("全局代理的密码被回显了: %q", shown)
	}
}

// 代理配错时更新链路退回全局代理，但要把原因报出去。
func TestUpdateSourceInfoSurfacesProxyConfigError(t *testing.T) {
	restoreUpdateSource(t)
	withUpdateProxy(t, "")
	SetUpdateProxySource("环境变量 ARTEX_UPDATE_PROXY", "不支持的代理协议 \"ftp\"")

	got := updateSourceInfo("http://127.0.0.1:7890")
	if got["proxy_error"] != "不支持的代理协议 \"ftp\"" {
		t.Errorf("proxy_error = %v", got["proxy_error"])
	}
	// 专用代理没生效，所以实际走的是全局代理，展示也必须这么说。
	if got["proxy_source"] != "全局出口代理 · 系统设置" {
		t.Errorf("proxy_source = %v", got["proxy_source"])
	}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 次查询只应回源 1 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 用户点「检查更新」必须拿到实时结果，否则刚发布的版本要等缓存过期才看得见。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force 应绕过缓存，期望回源 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 把落库时间往前拨到刚过期，模拟 TTL 到点。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 过期后应重新回源，期望 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 不可达")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	// 失败结果也要缓存一会儿，否则 GitHub 不可达时每次页面加载都白等一次超时。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 1 {
		t.Errorf("错误应短时缓存，期望回源 1 次，实际 %d 次", calls)
	}

	// 但错误的 TTL 必须明显短于成功的，网络恢复后要能很快自愈。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("错误 TTL(%v) 必须短于成功 TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 2 {
		t.Errorf("错误 TTL 过期后应重试，期望 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 访客关掉标签页会取消请求。那不代表 GitHub 有问题，绝不能把"已取消"
	// 写进缓存——否则接下来 30 分钟内每个访客都会收到一条莫名其妙的错误。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 让缓存过期，逼它回源

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("调用方已取消时应把错误透传给它")
	}

	// 关键不变量：被取消的那一次不留下任何痕迹——缓存里既没有"已取消"这个错误，
	// 也还保着上一次的好结果。
	if c.err != nil {
		t.Fatalf("取消错误不应写进缓存，得到 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("缓存应保留上一次的好结果，得到 %+v", c.rel)
	}

	// 那次取消没换来任何新数据，所以下一个访客理应重新回源——而且能正常拿到结果，
	// 不会被上一次的取消连累。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("取消之后的正常请求不应报错: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("应拿到正常结果，得到 %+v", rel)
	}
}
