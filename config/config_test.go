package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 发布源刻意只从环境变量/配置文件读，没有 HTTP setter——改它等于让服务器执行
// 任意代码（下载的二进制会替换 artex 本体）。这里锁住优先级和"没配就是空"的语义：
// 空值让调用方沿用官方源，而不是把发布源清成空串。
func TestUpdateRepoPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"update":{"repo":"from-file/artex"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", cfgPath)

	t.Setenv("ARTEX_UPDATE_REPO", "")
	repo, source := UpdateRepo()
	if repo != "from-file/artex" {
		t.Errorf("无环境变量时应取配置文件的值，得到 %q", repo)
	}
	if !strings.Contains(source, "update.repo") {
		t.Errorf("来源说明应指向配置文件的 update.repo，得到 %q", source)
	}

	// 环境变量优先：容器里改 compose 的 environment 比改挂进去的配置文件方便。
	t.Setenv("ARTEX_UPDATE_REPO", "from-env/artex")
	repo, source = UpdateRepo()
	if repo != "from-env/artex" {
		t.Errorf("环境变量应优先，得到 %q", repo)
	}
	if !strings.Contains(source, "ARTEX_UPDATE_REPO") {
		t.Errorf("来源说明应指向环境变量，得到 %q", source)
	}
}

func TestUpdateRepoEmptyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 只有数据库配置、没有 update 段——绝大多数现存部署就是这个样子。
	if err := os.WriteFile(cfgPath, []byte(`{"database":{"dsn":"postgres://x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", cfgPath)
	t.Setenv("ARTEX_UPDATE_REPO", "")

	if repo, _ := UpdateRepo(); repo != "" {
		t.Errorf("未配置时应返回空串（调用方沿用官方源），得到 %q", repo)
	}
	// 空白值等于没配，不能让它变成一个空的发布源。
	t.Setenv("ARTEX_UPDATE_REPO", "   ")
	if repo, _ := UpdateRepo(); repo != "" {
		t.Errorf("全空格应视为未配置，得到 %q", repo)
	}
}

// 更新代理和发布源配在一起：换了仓库往往也得换出网路径，拆到两处（一处文件一处
// 页面）容易出现"源改了但还从旧通道去取"的错配。
func TestUpdateProxyPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"update":{"proxy":"socks5://127.0.0.1:1080"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", cfgPath)

	t.Setenv("ARTEX_UPDATE_PROXY", "")
	proxy, source := UpdateProxy()
	if proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("无环境变量时应取配置文件的值，得到 %q", proxy)
	}
	if !strings.Contains(source, "update.proxy") {
		t.Errorf("来源说明应指向配置文件的 update.proxy，得到 %q", source)
	}

	t.Setenv("ARTEX_UPDATE_PROXY", "http://127.0.0.1:7890")
	proxy, source = UpdateProxy()
	if proxy != "http://127.0.0.1:7890" {
		t.Errorf("环境变量应优先，得到 %q", proxy)
	}
	if !strings.Contains(source, "ARTEX_UPDATE_PROXY") {
		t.Errorf("来源说明应指向环境变量，得到 %q", source)
	}
}

func TestUpdateProxyEmptyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"update":{"repo":"me/artex"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", cfgPath)
	t.Setenv("ARTEX_UPDATE_PROXY", "")

	// 只配了 repo 没配 proxy——空值表示沿用全局出口代理，而不是强制直连。
	if proxy, _ := UpdateProxy(); proxy != "" {
		t.Errorf("未配置时应返回空串，得到 %q", proxy)
	}
	t.Setenv("ARTEX_UPDATE_PROXY", "   ")
	if proxy, _ := UpdateProxy(); proxy != "" {
		t.Errorf("全空格应视为未配置，得到 %q", proxy)
	}
}

func TestPostgresDSNPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"database":{"host":"10.1.2.3","port":6000,"user":"u","password":"p","dbname":"d","sslmode":"require"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)

	// no env DSN → built from config file fields
	t.Setenv("ARTEX_PG_DSN", "")
	got, _, err := PostgresDSN()
	want := "postgres://u:p@10.1.2.3:6000/d?sslmode=require"
	if err != nil || got != want {
		t.Fatalf("from file: got %q err %v want %q", got, err, want)
	}

	// env wins over config file
	t.Setenv("ARTEX_PG_DSN", "postgres://envwins/x")
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://envwins/x" {
		t.Fatalf("env should win, got %q err %v", got, err)
	}

	// no env, no file → error (no built-in default)
	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "nope.json"))
	if got, _, err := PostgresDSN(); err == nil {
		t.Fatalf("missing config should error, got %q", got)
	}

	// full dsn in config file is used verbatim
	os.WriteFile(cfgPath, []byte(`{"database":{"dsn":"postgres://full/dsn"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://full/dsn" {
		t.Fatalf("file dsn verbatim, got %q err %v", got, err)
	}
}
