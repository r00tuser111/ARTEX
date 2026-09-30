package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultRepo 是官方发布源。
const DefaultRepo = "Autumn-27/artex"

// 发布源可换，但**只能由能登上服务器的人换**：SetRepo 的唯一调用方是 main，
// 取值来自环境变量 ARTEX_UPDATE_REPO 或 config.json，没有任何 HTTP 接口能改它。
//
// 这条边界是刻意的。一键更新会把下载到的二进制换装成 artex 本体，所以"能改发布源"
// 等于"能让服务器执行任意代码"；而本项目没有角色权限体系（auth.go 只有 JWT），
// 做成页面可配项就等于把任何一个 Web 登录态升级成了 RCE。
//
// 即使换了源，另外两道闸门照旧：URL 仍受 allowedHosts 白名单约束（所以只能是
// GitHub 上的另一个仓库，换不到自建域名去），下载仍必须对得上该 Release 的
// SHA256SUMS。
var (
	repoMu sync.RWMutex
	repo   = DefaultRepo
)

// Repo 返回当前发布源（owner/name）。
func Repo() string {
	repoMu.RLock()
	defer repoMu.RUnlock()
	return repo
}

// IsOfficialRepo 报告当前发布源是否官方源，供前端决定要不要标红警告。
func IsOfficialRepo() bool { return Repo() == DefaultRepo }

// SetRepo 校验并切换发布源。校验不通过时**不改动当前值**，调用方据此退回官方源。
// 只应在启动时、监听端口之前调用一次。
func SetRepo(raw string) error {
	v, err := ValidateRepo(raw)
	if err != nil {
		return err
	}
	repoMu.Lock()
	repo = v
	repoMu.Unlock()
	return nil
}

// maxRepoSegment 是 owner / name 各自的长度上限（GitHub 实际限制更严，这里只要
// 挡住畸形长输入即可）。
const maxRepoSegment = 100

// ValidateRepo 校验并规范化 "owner/name" 形式的发布源。
//
// 刻意只接受裸的 owner/name，不接受完整 URL、不剥离 https:// 前缀：这个值会被
// 拼进 api.github.com 的路径，越少的"智能解析"越少的绕过面。字符集用白名单而不是
// 黑名单，顺带把 %2e%2e 这类编码穿越挡在外面（'%' 不在白名单里）。
func ValidateRepo(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("发布源为空")
	}
	owner, name, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(name, "/") {
		return "", fmt.Errorf("发布源必须是 owner/name 形式（例如 %s），得到 %q", DefaultRepo, raw)
	}
	for _, seg := range [2]string{owner, name} {
		if err := checkRepoSegment(seg); err != nil {
			return "", fmt.Errorf("发布源 %q 无效: %w", raw, err)
		}
	}
	return owner + "/" + name, nil
}

func checkRepoSegment(seg string) error {
	if seg == "" {
		return errors.New("owner 和 name 都不能为空")
	}
	if len(seg) > maxRepoSegment {
		return fmt.Errorf("%q 超过 %d 个字符", seg, maxRepoSegment)
	}
	// "." / ".." 会把请求路径指到 /repos 之外。域名白名单挡不住这种情况——
	// 路径穿越之后主机名还是 api.github.com。
	if seg == "." || seg == ".." {
		return fmt.Errorf("%q 不是合法的路径片段", seg)
	}
	for _, c := range seg {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%q 含有不允许的字符 %q（只允许字母、数字、. _ -）", seg, c)
		}
	}
	return nil
}

// latestURL 是 GitHub 的"最新正式版"接口。它会自动跳过 prerelease 和 draft。
func latestURL() string {
	return "https://api.github.com/repos/" + Repo() + "/releases/latest"
}

// allowedHosts 限定升级链路能访问的域名。配合下面的 checkRedirect，
// 任何一跳被重定向到名单外的主机都会直接失败——这是防止 DNS 污染 / 中间人
// 把二进制换掉的第一道闸门，第二道是 SHA256SUMS 比对。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 资产实际落地的对象存储
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release 是 GitHub Release 里我们关心的字段。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset 是 Release 上挂的一个文件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// 更新链路专用代理。和发布源一样只认环境变量 ARTEX_UPDATE_PROXY / config.json，
// 原因也一样：发布源换了仓库，出网路径往往也得跟着换，两者配在一起才不会出现
// "源改了但还从旧通道去取"的错配。
//
// 空 = 沿用调用方传入的全局出口代理（系统设置里那个），保持既有部署的行为不变。
var (
	proxyMu     sync.RWMutex
	updateProxy string
)

// Proxy 返回更新链路专用代理（空表示未配置，沿用全局出口代理）。
func Proxy() string {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	return updateProxy
}

// SetProxy 校验并设置更新专用代理。空串清除它（更新链路退回全局出口代理），
// 与 Manager.SetGlobalProxy 的语义一致。校验不通过时不改动当前值。
// 只应在启动时调用一次。
func SetProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if _, err := ValidateProxyURL(raw); err != nil {
			return err
		}
	}
	proxyMu.Lock()
	updateProxy = raw
	proxyMu.Unlock()
	return nil
}

// ValidateProxyURL 校验代理地址，口径与 traffic.ValidateProxyURL 一致
// （http / https / socks5，必须带协议和主机）。刻意不 import traffic：selfupdate
// 在 Bootstrap 阶段就要跑，保持它只依赖标准库。
func ValidateProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("代理地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址 %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	case "":
		return nil, fmt.Errorf("代理 %q 缺少协议(用 http://、https:// 或 socks5://)", raw)
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q(用 http、https 或 socks5)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理 %q 缺少主机地址", raw)
	}
	return u, nil
}

// ResolveProxy 报告更新链路实际使用的代理。
// 优先级：更新专用代理 > fallback（全局出口代理） > 直连。
// dedicated 区分前两者，供页面说清"这个代理是从哪个开关来的"。
func ResolveProxy(fallback string) (proxy string, dedicated bool) {
	if p := Proxy(); p != "" {
		return p, true
	}
	return strings.TrimSpace(fallback), false
}

// RedactProxy 把代理地址里的密码换成 ****，供页面展示。
//
// 代理串常带 user:pass，而更新卡片对所有登录用户可见——原样回显等于把出网凭据
// 发给每个能打开设置页的人。解析不了就整串隐去，宁可显示不出来也不泄露。
func RedactProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	if u.User == nil || u.Scheme == "" {
		return u.String()
	}
	if _, hasPw := u.User.Password(); !hasPw {
		return u.String() // 只有用户名，没什么要遮的
	}
	// 刻意不走 url.UserPassword("****")：URL 编码会把掩码写成 %2A%2A%2A%2A，
	// 页面上看着像是密码本身的一部分。这里自己拼，只对用户名做转义。
	masked := url.User(u.User.Username()).String() + ":****"
	u.User = nil
	prefix := u.Scheme + "://"
	return prefix + masked + "@" + strings.TrimPrefix(u.String(), prefix)
}

// NewClient 构造一个只认 GitHub 域名的 HTTP 客户端。
//
// fallbackProxy 是调用方提供的兜底代理（全局出口代理）；配了更新专用代理时以后者
// 为准。两个都为空则直连。
//
// 刻意不复用默认 Transport：升级链路必须强制走 TLS 且校验证书，不能被别处
// 设置的 InsecureSkipVerify 之类影响到。
func NewClient(fallbackProxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p, _ := ResolveProxy(fallbackProxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 下载整包，不能按请求级超时卡死
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL 强制 https + 域名白名单。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("拒绝非 HTTPS 地址: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("拒绝非 GitHub 域名: %s", u.Hostname())
	}
	return nil
}

// FetchLatest 查询最新正式版。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL(), nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub 失败（可在系统设置里配置全局代理）: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 未认证的 GitHub API 是每 IP 每小时 60 次，共用出口 IP 时很容易撞上。
		return nil, fmt.Errorf("GitHub 接口限流（每小时 60 次），请稍后再试")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("仓库 %s 尚未发布任何正式版本", Repo())
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 返回 %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 缺少 tag")
	}
	return &rel, nil
}

// AssetName 返回当前平台对应的发布包名，与 build.sh 的 package_binary 保持一致：
// artex-<版本>-<os>-<arch>.zip（版本号不带 v 前缀）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset 在 Release 里按名字找资产。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
