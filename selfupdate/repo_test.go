package selfupdate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 发布源是这条链路上唯一可配的东西，而它会被拼进 api.github.com 的请求路径。
// 校验一旦漏，要么被路径穿越带去别的接口，要么被 URL 结构注入把主机名换掉——
// 之后装上的二进制会直接替换 artex 本体，所以这一组用例比其他测试更值得较真。

// withRepo 在测试结束后把包级发布源还原，避免污染同包其他测试。
func withRepo(t *testing.T, v string) {
	t.Helper()
	old := Repo()
	t.Cleanup(func() {
		repoMu.Lock()
		repo = old
		repoMu.Unlock()
	})
	if v != "" {
		if err := SetRepo(v); err != nil {
			t.Fatalf("SetRepo(%q): %v", v, err)
		}
	}
}

func TestValidateRepoAccepts(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Autumn-27/artex", "Autumn-27/artex"},
		{"a/b", "a/b"},
		{"  Autumn-27/artex  ", "Autumn-27/artex"}, // 从配置文件/环境变量里来的值常带空格
		{"me/my.repo", "me/my.repo"},               // 仓库名里的点是合法的
		{"some_org/some_repo", "some_org/some_repo"},
		{"Org-1/repo-2.0_x", "Org-1/repo-2.0_x"},
	}
	for _, c := range cases {
		got, err := ValidateRepo(c.in)
		if err != nil {
			t.Errorf("ValidateRepo(%q) 应当放行，却报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateRepoRejects(t *testing.T) {
	cases := []struct {
		in, why string
	}{
		{"", "空值"},
		{"   ", "只有空格"},
		{"onlyone", "缺少 owner/name 分隔"},
		{"a/b/c", "三段"},
		{"/b", "owner 为空"},
		{"a/", "name 为空"},
		{"/", "两段都为空"},
		// 路径穿越：域名白名单挡不住这类输入——穿越之后主机名还是 api.github.com，
		// 变的是请求的接口。
		{"../..", "两段都是 .."},
		{"a/..", "name 是 .."},
		{"../a", "owner 是 .."},
		{"./a", "owner 是 ."},
		{"a/.", "name 是 ."},
		// URL 结构注入：这些字符能改写请求的主机、路径或查询串。
		{"https://github.com/a/b", "完整 URL"},
		{"github.com/a/b", "带域名前缀"},
		{"a/b?x=1", "查询串"},
		{"a/b#frag", "片段"},
		{"a@b/c", "at 号"},
		{"user:pass@a/b", "凭据形式"},
		{"a/b\\c", "反斜杠"},
		// 编码绕过：'%' 不在白名单里，所以 %2e%2e 这类穿越在第一关就被挡住。
		{"a/%2e%2e", "编码的 .."},
		{"%2e%2e/a", "编码的 .."},
		{"a b/c", "空格"},
		{"a\t/b", "制表符"},
		{"a\n/b", "换行"},
		{"仓库/名字", "非 ASCII"},
		{strings.Repeat("a", maxRepoSegment+1) + "/b", "owner 超长"},
		{"a/" + strings.Repeat("b", maxRepoSegment+1), "name 超长"},
	}
	for _, c := range cases {
		if got, err := ValidateRepo(c.in); err == nil {
			t.Errorf("ValidateRepo(%q) 应当拒绝（%s），却返回 %q", c.in, c.why, got)
		}
	}
}

// 校验失败必须原样保留当前发布源。否则一个无效配置会把发布源清成空串，
// 拼出来的 URL 变成 /repos//releases/latest，错误信息还会指向莫名其妙的地方。
func TestSetRepoKeepsCurrentValueOnError(t *testing.T) {
	withRepo(t, "someone/fork")
	if err := SetRepo("not-a-repo"); err == nil {
		t.Fatal("SetRepo 对无效值应当报错")
	}
	if got := Repo(); got != "someone/fork" {
		t.Errorf("校验失败后发布源被改成了 %q，应保持 someone/fork", got)
	}
}

func TestSetRepoAffectsLatestURL(t *testing.T) {
	withRepo(t, "someone/fork")
	if got, want := latestURL(), "https://api.github.com/repos/someone/fork/releases/latest"; got != want {
		t.Errorf("latestURL() = %q, want %q", got, want)
	}
	// 换源之后主机名仍必须过得了白名单那一关——发布源只能是 GitHub 上的另一个
	// 仓库，换不到自建域名去。
	if err := checkURL(mustParse(t, latestURL())); err != nil {
		t.Errorf("换源后的 latestURL 应当仍能通过域名白名单: %v", err)
	}
}

func TestDefaultRepoIsDefault(t *testing.T) {
	if Repo() != DefaultRepo || !IsDefaultRepo() {
		t.Fatalf("默认发布源应为 %s，得到 %s", DefaultRepo, Repo())
	}
	if got, want := latestURL(), "https://api.github.com/repos/"+DefaultRepo+"/releases/latest"; got != want {
		t.Errorf("latestURL() = %q, want %q", got, want)
	}
}

// IsDefaultRepo 决定前端要不要标红警告并要求二次确认，所以它对"改成了别的源"
// 必须敏感，对"显式配成默认源"必须不报警。
func TestIsDefaultRepo(t *testing.T) {
	withRepo(t, "someone/fork")
	if IsDefaultRepo() {
		t.Error("非默认源不应被判为默认")
	}
	if err := SetRepo("  " + DefaultRepo + "  "); err != nil {
		t.Fatal(err)
	}
	if !IsDefaultRepo() {
		t.Error("显式配成默认源（带空格）应被判为默认")
	}
}

func TestValidateProxyURL(t *testing.T) {
	good := []string{
		"http://127.0.0.1:7890",
		"https://proxy.example.com:8443",
		"socks5://127.0.0.1:1080",
		"socks5://user:pass@127.0.0.1:1080",
		"  http://127.0.0.1:7890  ", // 配置文件里的值常带空格
	}
	for _, raw := range good {
		if _, err := ValidateProxyURL(raw); err != nil {
			t.Errorf("ValidateProxyURL(%q) 应当放行，却报错: %v", raw, err)
		}
	}
	bad := []struct{ in, why string }{
		{"", "空值"},
		{"127.0.0.1:7890", "缺少协议"},
		{"ftp://127.0.0.1:21", "不支持的协议"},
		{"socks4://127.0.0.1:1080", "不支持的协议"},
		{"http://", "缺少主机"},
	}
	for _, c := range bad {
		if _, err := ValidateProxyURL(c.in); err == nil {
			t.Errorf("ValidateProxyURL(%q) 应当拒绝（%s）", c.in, c.why)
		}
	}
}

// 专用代理必须盖过调用方传入的全局出口代理，没配时又必须老老实实退回去——
// 后者保证既有部署（只配了全局代理）的更新链路行为不变。
// 注：优先级逻辑已上移到 server.Manager.ResolveUpdateProxy，selfupdate 只负责
// 拿到最终的代理串去拨号，相关用例见 server 包。

// 代理地址会回显到更新卡片上，而那个页面对所有登录用户开放。密码泄出去就等于
// 把出网凭据（常常还是内网跳板的凭据）发给了每个能登录的人。
func TestRedactProxy(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"socks5://user@127.0.0.1:1080", "socks5://user@127.0.0.1:1080"}, // 只有用户名，无需遮
		{"socks5://user:s3cret@127.0.0.1:1080", "socks5://user:****@127.0.0.1:1080"},
		{"http://bob:hunter2@proxy.local:8080", "http://bob:****@proxy.local:8080"},
	}
	for _, c := range cases {
		if got := RedactProxy(c.in); got != c.want {
			t.Errorf("RedactProxy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 解析不了的串宁可整个隐去，也不能原样回显——万一里面就带着密码。
	if got := RedactProxy("http://a b:c@host"); got == "http://a b:c@host" {
		t.Errorf("无法解析的代理串不应原样回显，得到 %q", got)
	}
}

// NewClient 是这个功能真正的落点：传了代理就必须真的从那里出网。
func TestNewClientUsesProxy(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, latestURL(), nil)
	if err != nil {
		t.Fatal(err)
	}

	tr, ok := NewClient("socks5://127.0.0.1:1080").Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 类型不是 *http.Transport")
	}
	if tr.Proxy == nil {
		t.Fatal("配了代理时 Transport.Proxy 不应为 nil")
	}
	pu, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if pu == nil || pu.String() != "socks5://127.0.0.1:1080" {
		t.Errorf("客户端用的代理 = %v，want socks5://127.0.0.1:1080", pu)
	}

	// 传空串时必须直连：留一个空的 ProxyURL 会让请求发往空地址而彻底失败。
	tr, _ = NewClient("").Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("未传代理时 Transport.Proxy 应为 nil（直连）")
	}
	// 全空格同样按直连处理。
	tr, _ = NewClient("   ").Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("全空格代理应按直连处理，Transport.Proxy 应为 nil")
	}
}

// 光检查 Transport.Proxy 字段还不够——起一个本地代理，看更新请求是不是真的来敲它
// 的门。这里不需要外网：目标是 https，客户端会先向代理发 CONNECT，收到这一下就
// 证明流量确实走了代理，隧道之后建不起来无所谓。
func TestNewClientActuallyDialsProxy(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway) // 不真的建隧道
	}))
	defer proxy.Close()

	// 请求必然失败（代理不给建隧道），我们要的是"代理被访问到了"这个事实。
	resp, err := NewClient(proxy.URL).Get(latestURL())
	if err == nil {
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("配了代理，但请求没有经过它")
	}
	if want := "CONNECT api.github.com:443"; seen[0] != want {
		t.Errorf("代理收到 %q, want %q", seen[0], want)
	}
}
