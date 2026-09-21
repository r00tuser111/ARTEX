"use client";

import * as React from "react";

import { Bot, Check, Copy, Loader2, Plus, RefreshCw, ShieldCheck, Trash2, UserCheck, X } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { RemoteBinding, RemoteChannel, RemotePairingRequest } from "@/lib/types";

type ChannelDraft = {
  id?: number;
  kind: "wechat_claw" | "feishu";
  name: string;
  enabled: boolean;
  app_id: string;
  app_secret: string;
  verification_token: string;
  encrypt_key: string;
  base_url: string;
  route_tag: string;
};

const emptyDraft = (): ChannelDraft => ({
  kind: "wechat_claw",
  name: "微信 Claw",
  enabled: true,
  app_id: "",
  app_secret: "",
  verification_token: "",
  encrypt_key: "",
  base_url: "https://ilinkai.weixin.qq.com",
  route_tag: "",
});

async function copyText(text: string, label = "已复制") {
  await navigator.clipboard.writeText(text);
  toast.success(label);
}

function wechatLoginStatus(status: string) {
  return (
    {
      wait: "等待扫码",
      scaned: "已扫码，正在验证",
      scaned_but_redirect: "已扫码，正在切换接入点",
      need_verifycode: "需要输入手机显示的验证码",
      verify_code_blocked: "验证码错误次数过多，请刷新二维码",
      binded_redirect: "此微信 Claw 已绑定；如本渠道未连接，请先在原客户端解绑",
      expired: "二维码已过期",
      confirmed: "登录成功",
    }[status] ?? status
  );
}

export default function RemoteControlPage() {
  const [channels, setChannels] = React.useState<RemoteChannel[]>([]);
  const [bindings, setBindings] = React.useState<RemoteBinding[]>([]);
  const [pairingRequests, setPairingRequests] = React.useState<RemotePairingRequest[]>([]);
  const [draft, setDraft] = React.useState<ChannelDraft>(emptyDraft);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [wechatLogin, setWechatLogin] = React.useState({ image: "", status: "", verifyCode: "", error: "" });
  const pollingWechatLogin = React.useRef(false);

  const reload = React.useCallback(async () => {
    const [cs, bs, requests] = await Promise.all([
      api.remoteChannels(),
      api.remoteBindings(),
      api.remotePairingRequests(),
    ]);
    setChannels(cs);
    setBindings(bs);
    setPairingRequests(requests);
  }, []);

  React.useEffect(() => {
    reload()
      .catch((e) => toast.error(`加载失败：${(e as Error).message}`))
      .finally(() => setLoading(false));
  }, [reload]);

  const choose = React.useCallback((c: RemoteChannel) => {
    setDraft({
      id: c.id,
      kind: c.kind,
      name: c.name,
      enabled: c.enabled,
      app_id: c.app_id ?? "",
      app_secret: "",
      verification_token: "",
      encrypt_key: "",
      base_url: c.base_url ?? (c.kind === "feishu" ? "https://open.feishu.cn" : "https://ilinkai.weixin.qq.com"),
      route_tag: c.route_tag ?? "",
    });
    setWechatLogin({ image: "", status: "", verifyCode: "", error: "" });
  }, []);

  const startNew = (kind: "wechat_claw" | "feishu") => {
    const next = emptyDraft();
    next.kind = kind;
    next.name = kind === "feishu" ? "飞书" : "微信 Claw";
    next.base_url = kind === "feishu" ? "https://open.feishu.cn" : "https://ilinkai.weixin.qq.com";
    setDraft(next);
    setWechatLogin({ image: "", status: "", verifyCode: "", error: "" });
  };

  const save = async () => {
    setSaving(true);
    try {
      const result = await api.saveRemoteChannel(draft);
      await reload();
      choose(result.channel);
      toast.success(draft.id ? "渠道配置已更新" : "远程渠道已创建");
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  };

  const startWechatLogin = async (channelId: number) => {
    try {
      const result = await api.startWeChatLogin(channelId);
      setWechatLogin({ image: result.qrcode_img_content, status: result.status, verifyCode: "", error: "" });
      toast.success("请使用微信扫描二维码");
    } catch (e) {
      const message = (e as Error).message;
      setWechatLogin((current) => ({ ...current, error: message }));
      toast.error(`获取二维码失败：${message}`);
    }
  };

  const pollWechatLogin = React.useCallback(async () => {
    if (!draft.id || pollingWechatLogin.current) return;
    pollingWechatLogin.current = true;
    try {
      const result = await api.pollWeChatLogin(draft.id, wechatLogin.verifyCode);
      setWechatLogin((current) => ({ ...current, status: result.status, error: "" }));
      if (result.status === "confirmed" || (result.status === "binded_redirect" && result.channel.connected)) {
        await reload();
        choose(result.channel);
        toast.success("微信 Claw 已连接");
      }
    } catch (e) {
      setWechatLogin((current) => ({ ...current, error: (e as Error).message }));
    } finally {
      pollingWechatLogin.current = false;
    }
  }, [draft.id, wechatLogin.verifyCode, reload, choose]);

  React.useEffect(() => {
    if (!wechatLogin.image || !["wait", "scaned", "scaned_but_redirect"].includes(wechatLogin.status)) return;
    const timer = window.setInterval(() => void pollWechatLogin(), 2000);
    return () => window.clearInterval(timer);
  }, [wechatLogin.image, wechatLogin.status, pollWechatLogin]);

  const removeChannel = async (c: RemoteChannel) => {
    if (!window.confirm(`删除「${c.name}」及其全部远程绑定？此操作不可撤销。`)) return;
    try {
      await api.deleteRemoteChannel(c.id);
      if (draft.id === c.id) setDraft(emptyDraft());
      await reload();
      toast.success("远程渠道已删除");
    } catch (e) {
      toast.error(`删除失败：${(e as Error).message}`);
    }
  };

  const approvePairing = async (request: RemotePairingRequest) => {
    try {
      await api.approveRemotePairing(request.id);
      await reload();
      toast.success(`已批准配对码 ${request.code}`);
    } catch (e) {
      toast.error(`批准失败：${(e as Error).message}`);
    }
  };

  const dismissPairing = async (request: RemotePairingRequest) => {
    try {
      await api.dismissRemotePairing(request.id);
      await reload();
      toast.success("配对请求已拒绝");
    } catch (e) {
      toast.error(`拒绝失败：${(e as Error).message}`);
    }
  };

  const removeBinding = async (binding: RemoteBinding) => {
    if (!window.confirm(`解除远程用户 ${binding.display_name || binding.external_user_id} 的绑定？`)) return;
    try {
      await api.deleteRemoteBinding(binding.id);
      await reload();
      toast.success("绑定已解除");
    } catch (e) {
      toast.error(`解绑失败：${(e as Error).message}`);
    }
  };

  const selected = channels.find((c) => c.id === draft.id);
  const selectedBindings = draft.id ? bindings.filter((b) => b.channel_id === draft.id) : [];
  const selectedPairingRequests = draft.id ? pairingRequests.filter((request) => request.channel_id === draft.id) : [];

  return (
    <div className="flex flex-1 flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">远程控制</h1>
          <p className="text-sm text-muted-foreground">通过微信 Claw、飞书机器人等对话渠道管理任务并查询执行状态。</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => startNew("wechat_claw")}>
            <Plus className="mr-2 size-4" /> 微信 Claw
          </Button>
          <Button variant="outline" onClick={() => startNew("feishu")}>
            <Plus className="mr-2 size-4" /> 飞书
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheck className="size-4" /> 安全模型
          </CardTitle>
          <CardDescription>
            微信 Claw 使用官方 iLink 登录态，扫码用户会自动获得控制权限。其他陌生账号首次发消息时，机器人会返回 8
            位配对码，管理员核对后在本页批准。每个远程会话独立保存并支持消息去重，登录凭据不会通过管理 API 回显。
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          快捷命令：<code>/tasks</code>、<code>/task ID</code>、<code>/pause ID</code>、<code>/resume ID</code>、
          <code>/new 描述 | 目标</code>。其他自然语言会交给 Auto Agent 调用任务编排工具。
        </CardContent>
      </Card>

      <div className="grid gap-6 xl:grid-cols-[minmax(260px,0.8fr)_minmax(420px,1.4fr)]">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">已配置渠道</CardTitle>
            <CardDescription>选择渠道编辑配置、扫码连接或审批配对请求。</CardDescription>
          </CardHeader>
          <CardContent className="space-y-2">
            {loading ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
                加载中
              </div>
            ) : channels.length === 0 ? (
              <p className="text-sm text-muted-foreground">尚未配置远程渠道。</p>
            ) : (
              channels.map((c) => (
                <button
                  type="button"
                  key={c.id}
                  onClick={() => choose(c)}
                  className={`flex w-full items-center justify-between rounded-md border p-3 text-left transition-colors hover:bg-muted/50 ${draft.id === c.id ? "border-primary bg-muted/50" : ""}`}
                >
                  <span>
                    <span className="block text-sm font-medium">{c.name}</span>
                    <span className="text-xs text-muted-foreground">
                      {c.kind === "feishu" ? "飞书应用机器人" : "微信 Claw（iLink）"}
                    </span>
                  </span>
                  <Badge variant={c.enabled ? "default" : "secondary"}>{c.enabled ? "启用" : "停用"}</Badge>
                </button>
              ))
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <Bot className="size-4" />
              {draft.id ? "编辑渠道" : "新建渠道"}
            </CardTitle>
            <CardDescription>
              {draft.kind === "wechat_claw"
                ? "直接使用腾讯微信 Claw 的 iLink 协议扫码登录、接收消息并回复，无需额外中间层。"
                : "飞书开放平台使用事件回调接收消息，ARTEX 通过应用机器人直接回复。"}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <Label>类型</Label>
                <Select
                  value={draft.kind}
                  disabled={!!draft.id}
                  onValueChange={(value: "wechat_claw" | "feishu") => setDraft((d) => ({ ...d, kind: value }))}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="wechat_claw">微信 Claw</SelectItem>
                    <SelectItem value="feishu">飞书</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="remote-name">名称</Label>
                <Input
                  id="remote-name"
                  value={draft.name}
                  onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
                />
              </div>
            </div>

            <div className="flex items-center justify-between rounded-md border p-3">
              <div>
                <Label htmlFor="remote-enabled">启用渠道</Label>
                <p className="text-xs text-muted-foreground">停用后停止微信长轮询，或拒绝新的飞书回调。</p>
              </div>
              <Switch
                id="remote-enabled"
                checked={draft.enabled}
                onCheckedChange={(enabled) => setDraft((d) => ({ ...d, enabled }))}
              />
            </div>

            {draft.kind === "wechat_claw" ? (
              <div className="space-y-4">
                <div className="space-y-2">
                  <Label>iLink API 地址</Label>
                  <Input
                    value={draft.base_url}
                    onChange={(e) => setDraft((d) => ({ ...d, base_url: e.target.value }))}
                  />
                </div>
                <div className="space-y-2">
                  <Label>SKRouteTag（可选）</Label>
                  <Input
                    value={draft.route_tag}
                    onChange={(e) => setDraft((d) => ({ ...d, route_tag: e.target.value }))}
                  />
                </div>
                <div className="rounded-md border p-3 text-sm">
                  <div className="flex items-center justify-between gap-3">
                    <div>
                      <p className="font-medium">微信连接</p>
                      <p className="text-xs text-muted-foreground">
                        {selected?.connected ? `已连接 ${selected.ilink_bot_id || "微信 Claw"}` : "尚未扫码登录"}
                      </p>
                    </div>
                    <Badge variant={selected?.connected ? "default" : "secondary"}>
                      {selected?.connected ? "已连接" : "未连接"}
                    </Badge>
                  </div>
                  <Button
                    className="mt-3"
                    type="button"
                    variant="outline"
                    disabled={!selected}
                    onClick={() => selected && startWechatLogin(selected.id)}
                  >
                    {selected?.connected ? "重新扫码登录" : "微信扫码登录"}
                  </Button>
                  {!selected && <p className="mt-2 text-xs text-muted-foreground">请先保存渠道，再扫码连接。</p>}
                </div>
              </div>
            ) : (
              <div className="space-y-4">
                <div className="space-y-2">
                  <Label>App ID</Label>
                  <Input value={draft.app_id} onChange={(e) => setDraft((d) => ({ ...d, app_id: e.target.value }))} />
                </div>
                <div className="space-y-2">
                  <Label>App Secret {selected?.app_secret_set ? "（已配置；留空保持不变）" : ""}</Label>
                  <Input
                    type="password"
                    autoComplete="new-password"
                    value={draft.app_secret}
                    onChange={(e) => setDraft((d) => ({ ...d, app_secret: e.target.value }))}
                  />
                </div>
                <div className="space-y-2">
                  <Label>Verification Token {selected?.verification_token_set ? "（已配置；留空保持不变）" : ""}</Label>
                  <Input
                    type="password"
                    value={draft.verification_token}
                    onChange={(e) => setDraft((d) => ({ ...d, verification_token: e.target.value }))}
                  />
                </div>
                <div className="space-y-2">
                  <Label>Encrypt Key（可选，用于请求签名校验）</Label>
                  <Input
                    type="password"
                    value={draft.encrypt_key}
                    onChange={(e) => setDraft((d) => ({ ...d, encrypt_key: e.target.value }))}
                  />
                </div>
                <div className="space-y-2">
                  <Label>开放平台地址</Label>
                  <Input
                    value={draft.base_url}
                    onChange={(e) => setDraft((d) => ({ ...d, base_url: e.target.value }))}
                  />
                </div>
              </div>
            )}

            <div className="flex flex-wrap gap-2">
              <Button onClick={save} disabled={saving}>
                {saving ? <Loader2 className="mr-2 size-4 animate-spin" /> : <Check className="mr-2 size-4" />}保存
              </Button>
              {selected && (
                <Button variant="destructive" onClick={() => removeChannel(selected)}>
                  <Trash2 className="mr-2 size-4" />
                  删除
                </Button>
              )}
            </div>

            {wechatLogin.image && selected?.kind === "wechat_claw" && (
              <div className="space-y-3 rounded-md border border-primary/30 bg-primary/5 p-4 text-sm">
                <p className="font-medium">使用微信扫描二维码并在手机端确认</p>
                {/* The provider returns a short-lived signed URL on a dynamic Weixin host. */}
                {/* biome-ignore lint/performance/noImgElement: Next Image rejects the provider's dynamic QR host. */}
                <img
                  className="size-56 rounded-md bg-white object-contain p-2"
                  src={wechatLogin.image}
                  alt="微信 Claw 登录二维码"
                  width={224}
                  height={224}
                />
                <p className="text-muted-foreground">登录状态：{wechatLoginStatus(wechatLogin.status || "wait")}</p>
                {wechatLogin.error && <p className="text-destructive">连接错误：{wechatLogin.error}</p>}
                {wechatLogin.status === "need_verifycode" && (
                  <div className="flex gap-2">
                    <Input
                      placeholder="输入微信显示的验证码"
                      value={wechatLogin.verifyCode}
                      onChange={(e) => setWechatLogin((current) => ({ ...current, verifyCode: e.target.value }))}
                    />
                    <Button
                      type="button"
                      onClick={() => pollWechatLogin().catch((e) => toast.error((e as Error).message))}
                    >
                      验证
                    </Button>
                  </div>
                )}
                {["expired", "verify_code_blocked"].includes(wechatLogin.status) && (
                  <Button type="button" variant="outline" onClick={() => startWechatLogin(selected.id)}>
                    刷新二维码
                  </Button>
                )}
              </div>
            )}

            {selected?.kind === "feishu" && (
              <div className="space-y-2 rounded-md border p-3">
                <Label>Webhook URL</Label>
                <div className="flex gap-2">
                  <Input readOnly value={selected.webhook_url} />
                  <Button
                    size="icon"
                    variant="outline"
                    onClick={() => copyText(selected.webhook_url, "Webhook URL 已复制")}
                  >
                    <Copy className="size-4" />
                  </Button>
                </div>
                <p className="text-xs text-muted-foreground">
                  在飞书事件订阅中填写此地址，并订阅 im.message.receive_v1；需要开启机器人收发消息权限。
                </p>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {selected && (
        <Card>
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle className="text-base">待审批配对</CardTitle>
              <CardDescription>陌生用户首次发消息后，机器人会把同一组 8 位配对码发给用户。</CardDescription>
            </div>
            <Button size="sm" variant="outline" onClick={() => reload()}>
              <RefreshCw className="mr-2 size-4" />
              刷新
            </Button>
          </CardHeader>
          <CardContent>
            {selectedPairingRequests.length === 0 ? (
              <p className="text-sm text-muted-foreground">暂无待审批的配对请求。</p>
            ) : (
              <div className="space-y-2">
                {selectedPairingRequests.map((request) => (
                  <div
                    key={request.id}
                    className="flex flex-wrap items-center justify-between gap-3 rounded-md border p-3"
                  >
                    <div>
                      <p className="text-sm font-medium">
                        {request.display_name || request.external_user_id} · 配对码 {request.code}
                      </p>
                      <p className="text-xs text-muted-foreground">
                        用户 {request.external_user_id} · 会话 {request.external_chat_id || "私聊"} · 1 小时内有效
                      </p>
                    </div>
                    <div className="flex gap-2">
                      <Button size="sm" onClick={() => approvePairing(request)}>
                        <UserCheck className="mr-2 size-4" />
                        批准
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => dismissPairing(request)}>
                        <X className="mr-2 size-4" />
                        拒绝
                      </Button>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {selected && (
        <Card>
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle className="text-base">已绑定账号</CardTitle>
              <CardDescription>解除绑定不会删除对应的审计会话。</CardDescription>
            </div>
            <Button size="sm" variant="outline" onClick={() => reload()}>
              <RefreshCw className="mr-2 size-4" />
              刷新
            </Button>
          </CardHeader>
          <CardContent>
            {selectedBindings.length === 0 ? (
              <p className="text-sm text-muted-foreground">尚无账号完成配对。</p>
            ) : (
              <div className="space-y-2">
                {selectedBindings.map((b) => (
                  <div key={b.id} className="flex flex-wrap items-center justify-between gap-3 rounded-md border p-3">
                    <div>
                      <p className="text-sm font-medium">{b.display_name || b.external_user_id}</p>
                      <p className="text-xs text-muted-foreground">
                        用户 {b.external_user_id} · 会话 {b.external_chat_id || "私聊"} · ARTEX 对话 #
                        {b.conversation_id}
                      </p>
                    </div>
                    <Button size="sm" variant="ghost" onClick={() => removeBinding(b)}>
                      <Trash2 className="mr-2 size-4" />
                      解绑
                    </Button>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
