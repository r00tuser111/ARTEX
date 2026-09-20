package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

const (
	remoteMaxBody               = 256 << 10
	remotePairingTTL            = 10 * time.Minute
	remoteDefaultAgent          = "auto"
	weChatDefaultBaseURL        = "https://ilinkai.weixin.qq.com"
	weChatChannelVersion        = "2.4.9"
	weChatDefaultLongPoll       = 35 * time.Second
	weChatMaximumReplyRuneCount = 12000
)

type remoteChannelConfig struct {
	AppID             string `json:"app_id,omitempty"`
	AppSecret         string `json:"app_secret,omitempty"`
	VerificationToken string `json:"verification_token,omitempty"`
	EncryptKey        string `json:"encrypt_key,omitempty"`
	BaseURL           string `json:"base_url,omitempty"`
	RouteTag          string `json:"route_tag,omitempty"`
	BotToken          string `json:"bot_token,omitempty"`
	ILinkBotID        string `json:"ilink_bot_id,omitempty"`
	ILinkUserID       string `json:"ilink_user_id,omitempty"`
	QRCode            string `json:"qr_code,omitempty"`
	QRBaseURL         string `json:"qr_base_url,omitempty"`
}

type remoteChannelDTO struct {
	ID                   int64     `json:"id"`
	EndpointKey          string    `json:"endpoint_key"`
	Kind                 string    `json:"kind"`
	Name                 string    `json:"name"`
	Enabled              bool      `json:"enabled"`
	WebhookURL           string    `json:"webhook_url,omitempty"`
	Connected            bool      `json:"connected"`
	ILinkBotID           string    `json:"ilink_bot_id,omitempty"`
	ILinkUserID          string    `json:"ilink_user_id,omitempty"`
	RouteTag             string    `json:"route_tag,omitempty"`
	AppID                string    `json:"app_id,omitempty"`
	AppSecretSet         bool      `json:"app_secret_set,omitempty"`
	VerificationTokenSet bool      `json:"verification_token_set,omitempty"`
	EncryptKeySet        bool      `json:"encrypt_key_set,omitempty"`
	BaseURL              string    `json:"base_url,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func randomRemoteSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func remoteSecretHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Server) remotePairingHash(code string) string {
	h := hmac.New(sha256.New, s.jwtKey)
	_, _ = h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}

func decodeRemoteConfig(c *db.RemoteChannel) remoteChannelConfig {
	var cfg remoteChannelConfig
	if c != nil {
		_ = json.Unmarshal(c.Config, &cfg)
	}
	return cfg
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if h := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); h != "" {
		host = h
	}
	return scheme + "://" + host
}

func remoteWebhookPath(c *db.RemoteChannel) string {
	return "/api/remote/hooks/" + c.Kind + "/" + c.EndpointKey
}

func remoteChannelView(r *http.Request, c *db.RemoteChannel) remoteChannelDTO {
	cfg := decodeRemoteConfig(c)
	webhookURL := ""
	if c.Kind == "feishu" {
		webhookURL = requestBaseURL(r) + remoteWebhookPath(c)
	}
	return remoteChannelDTO{
		ID: c.ID, EndpointKey: c.EndpointKey, Kind: c.Kind, Name: c.Name, Enabled: c.Enabled,
		WebhookURL: webhookURL, Connected: cfg.BotToken != "", ILinkBotID: cfg.ILinkBotID,
		ILinkUserID: cfg.ILinkUserID, RouteTag: cfg.RouteTag,
		AppID: cfg.AppID, AppSecretSet: cfg.AppSecret != "", VerificationTokenSet: cfg.VerificationToken != "",
		EncryptKeySet: cfg.EncryptKey != "", BaseURL: cfg.BaseURL, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (s *Server) listRemoteChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.ListRemoteChannels()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]remoteChannelDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, remoteChannelView(r, c))
	}
	writeJSON(w, 200, map[string]any{"channels": out})
}

func (s *Server) saveRemoteChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                int64  `json:"id"`
		Kind              string `json:"kind"`
		Name              string `json:"name"`
		Enabled           *bool  `json:"enabled"`
		AppID             string `json:"app_id"`
		AppSecret         string `json:"app_secret"`
		VerificationToken string `json:"verification_token"`
		EncryptKey        string `json:"encrypt_key"`
		BaseURL           string `json:"base_url"`
		RouteTag          string `json:"route_tag"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, remoteMaxBody)
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Name = strings.TrimSpace(req.Name)
	if req.Kind != "wechat_claw" && req.Kind != "feishu" {
		writeErr(w, 400, "kind 必须为 wechat_claw 或 feishu")
		return
	}
	if req.Name == "" {
		req.Name = map[string]string{"wechat_claw": "微信 Claw", "feishu": "飞书"}[req.Kind]
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var existing *db.RemoteChannel
	var err error
	if req.ID > 0 {
		existing, err = s.m.pg.GetRemoteChannel(req.ID)
		if err != nil || existing == nil {
			writeErr(w, 404, "远程渠道不存在")
			return
		}
		if existing.Kind != req.Kind {
			writeErr(w, 400, "渠道类型创建后不能修改")
			return
		}
	}
	cfg := decodeRemoteConfig(existing)
	if strings.TrimSpace(req.AppID) != "" {
		cfg.AppID = strings.TrimSpace(req.AppID)
	}
	if req.AppSecret != "" {
		cfg.AppSecret = req.AppSecret
	}
	if req.VerificationToken != "" {
		cfg.VerificationToken = req.VerificationToken
	}
	if req.EncryptKey != "" {
		cfg.EncryptKey = req.EncryptKey
	}
	if strings.TrimSpace(req.BaseURL) != "" {
		cfg.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	}
	if req.Kind == "wechat_claw" {
		cfg.RouteTag = strings.TrimSpace(req.RouteTag)
		if cfg.BaseURL == "" {
			cfg.BaseURL = weChatDefaultBaseURL
		}
	}
	if cfg.BaseURL == "" && req.Kind == "feishu" {
		cfg.BaseURL = "https://open.feishu.cn"
	}
	if req.Kind == "feishu" && (cfg.AppID == "" || cfg.AppSecret == "" || cfg.VerificationToken == "") {
		writeErr(w, 400, "飞书渠道需要 app_id、app_secret 和 verification_token")
		return
	}
	configJSON, _ := json.Marshal(cfg)
	var saved *db.RemoteChannel
	if existing == nil {
		endpoint, e := randomRemoteSecret(18)
		if e != nil {
			writeErr(w, 500, e.Error())
			return
		}
		saved, err = s.m.pg.CreateRemoteChannel(endpoint, req.Kind, req.Name, enabled, configJSON)
	} else {
		saved, err = s.m.pg.UpdateRemoteChannel(req.ID, req.Name, enabled, configJSON)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.restartWeChatChannel(saved)
	writeJSON(w, 200, map[string]any{"channel": remoteChannelView(r, saved)})
}

func (s *Server) deleteRemoteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad channel id")
		return
	}
	s.stopWeChatChannel(id)
	if err := s.m.pg.DeleteRemoteChannel(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) createRemotePairingCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad channel id")
		return
	}
	if c, _ := s.m.pg.GetRemoteChannel(id); c == nil {
		writeErr(w, 404, "远程渠道不存在")
		return
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n := (int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])) & 0x7fffffff
	code := fmt.Sprintf("%06d", n%1000000)
	expires := time.Now().Add(remotePairingTTL)
	if err := s.m.pg.CreateRemotePairingCode(id, s.remotePairingHash(code), expires); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expires_at": expires, "command": "/bind " + code})
}

func (s *Server) listRemoteBindings(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	rows, err := s.m.pg.ListRemoteBindings(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"bindings": rows})
}

func (s *Server) deleteRemoteBinding(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad binding id")
		return
	}
	if err := s.m.pg.DeleteRemoteBinding(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

type remoteInbound struct {
	MessageID  string `json:"message_id"`
	SenderID   string `json:"sender_id"`
	ChatID     string `json:"chat_id"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
}

func (s *Server) enqueueRemoteMessage(c *db.RemoteChannel, in remoteInbound) (*db.RemoteMessage, bool, error) {
	id, err := randomRemoteSecret(18)
	if err != nil {
		return nil, false, err
	}
	binding, err := s.m.pg.GetRemoteBinding(c.ID, in.SenderID, in.ChatID)
	if err != nil {
		return nil, false, err
	}
	var conv *int64
	if binding != nil {
		conv = &binding.ConversationID
	}
	return s.m.pg.CreateRemoteMessage(&db.RemoteMessage{ID: id, ChannelID: c.ID, ExternalMessageID: in.MessageID,
		ExternalUserID: in.SenderID, ExternalChatID: in.ChatID, ConversationID: conv, RequestText: in.Text})
}

func (s *Server) processRemoteMessage(c *db.RemoteChannel, job *db.RemoteMessage, in remoteInbound, deliver func(string) error) {
	reply, err := s.executeRemoteMessage(c, job, in)
	status, errText := "completed", ""
	if err != nil {
		status, errText = "failed", err.Error()
		if reply == "" {
			reply = "远程指令执行失败：" + err.Error()
		}
	}
	if e := s.m.pg.FinishRemoteMessage(job.ID, status, reply, errText); e != nil {
		log.Printf("[remote] finish job %s: %v", job.ID, e)
	}
	if deliver != nil {
		if e := deliver(reply); e != nil {
			log.Printf("[remote] deliver job %s: %v", job.ID, e)
		}
	}
}

func (s *Server) executeRemoteMessage(c *db.RemoteChannel, job *db.RemoteMessage, in remoteInbound) (string, error) {
	if code, ok := parsePairCommand(in.Text); ok {
		if existing, err := s.m.pg.GetRemoteBinding(c.ID, in.SenderID, in.ChatID); err != nil {
			return "", err
		} else if existing != nil {
			_ = s.m.pg.StartRemoteMessage(job.ID, existing.ConversationID)
			return "此账号已经绑定，无需重复配对。发送 /help 查看可用命令。", nil
		}
		matched, err := s.m.pg.ConsumeRemotePairingCode(c.ID, s.remotePairingHash(code))
		if err != nil {
			return "", err
		}
		if !matched {
			return "配对码无效、已使用或已过期。请在 ARTEX「远程控制」页面重新生成。", nil
		}
		conv, err := s.m.pg.CreateConversation(remoteDefaultAgent, c.Name+" · "+in.SenderID, nil)
		if err != nil {
			return "", err
		}
		if _, err := s.m.pg.CreateRemoteBinding(c.ID, in.SenderID, in.ChatID, in.SenderName, conv.ID); err != nil {
			return "", err
		}
		_ = s.m.pg.StartRemoteMessage(job.ID, conv.ID)
		return "绑定成功。现在可直接用自然语言管理 ARTEX；发送 /help 查看快捷命令。", nil
	}
	binding, err := s.m.pg.GetRemoteBinding(c.ID, in.SenderID, in.ChatID)
	if err != nil {
		return "", err
	}
	if binding == nil {
		return "此账号尚未绑定 ARTEX。请先在网页「远程控制」中生成配对码，然后发送：/bind 123456", nil
	}
	if err := s.m.pg.StartRemoteMessage(job.ID, binding.ConversationID); err != nil {
		return "", err
	}
	if reply, handled, err := s.remoteCommand(in.Text); handled {
		// Deterministic commands bypass the LLM, but still appear in the bound
		// conversation so remote control has the same human-readable audit trail.
		ua := userActivityWithAttachments(remoteDefaultAgent, in.Text, nil)
		ua.Summary, ua.Detail = firstLine(in.Text, 200), in.Text
		_, _ = s.m.pg.AppendConvActivity(binding.ConversationID, ua)
		if err != nil {
			_, _ = s.m.pg.AppendConvActivity(binding.ConversationID, db.Activity{Worker: remoteDefaultAgent, Kind: "text", IsError: true, Summary: err.Error(), Detail: err.Error()})
		} else {
			_, _ = s.m.pg.AppendConvActivity(binding.ConversationID, db.Activity{Worker: remoteDefaultAgent, Kind: "text", Summary: reply, Detail: reply})
		}
		_ = s.m.pg.TouchConversation(binding.ConversationID)
		return reply, err
	}
	return s.runRemoteAutoConversation(binding.ConversationID, in.Text)
}

func parsePairCommand(text string) (string, bool) {
	f := strings.Fields(strings.TrimSpace(text))
	if len(f) == 2 && (strings.EqualFold(f[0], "/bind") || f[0] == "绑定") {
		return f[1], true
	}
	return "", false
}

func (s *Server) remoteCommand(text string) (string, bool, error) {
	f := strings.Fields(strings.TrimSpace(text))
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return "", false, nil
	}
	switch strings.ToLower(f[0]) {
	case "/help":
		return "快捷命令：\n/tasks — 任务列表\n/task <ID> — 任务详情\n/pause <ID> — 暂停任务\n/resume <ID> — 恢复任务\n/new <描述> | <目标> — 新建任务\n其余内容会交给 Auto 操作助手理解并执行。", true, nil
	case "/tasks":
		rows := s.m.List()
		if len(rows) == 0 {
			return "当前没有任务。", true, nil
		}
		var b strings.Builder
		b.WriteString("当前任务：")
		for _, t := range rows {
			fmt.Fprintf(&b, "\n#%s [%s] %s", t.ID, s.resolvedTaskStatus(t), t.Description)
		}
		return b.String(), true, nil
	case "/task":
		if len(f) != 2 {
			return "用法：/task <任务ID>", true, nil
		}
		t, ok := s.m.Task(f[1])
		if !ok {
			return "任务不存在：#" + f[1], true, nil
		}
		st, _ := t.Store.Stats()
		goals, _ := t.Store.ListByKind(db.KindGoal, 1000)
		findings, _ := t.Store.ListByKind(db.KindFinding, 1000)
		return fmt.Sprintf("#%s [%s] %s\n目标：%s\n子目标：%d，发现：%d\n图谱：%v", t.ID, s.resolvedTaskStatus(t), t.Description, t.Goal, len(goals), len(findings), st), true, nil
	case "/pause", "/resume":
		if len(f) != 2 {
			return "用法：" + f[0] + " <任务ID>", true, nil
		}
		t, ok := s.m.Task(f[1])
		if !ok {
			return "任务不存在：#" + f[1], true, nil
		}
		action := strings.TrimPrefix(strings.ToLower(f[0]), "/")
		result, err := s.applyTaskControlWithCause(t, action, agent.AbortPausedByOrchestrator)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("任务 #%s 已%s（状态：%s）", t.ID, map[string]string{"pause": "暂停", "resume": "恢复"}[action], result.Status), true, nil
	case "/new":
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), f[0]))
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			return "用法：/new <任务描述> | <任务目标>", true, nil
		}
		t, err := s.m.CreateTaskWithOptions(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), db.TaskCreateOptions{})
		if err != nil {
			return "", true, err
		}
		s.launchTask(t, t.Description+" "+t.Goal, false)
		return "已新建并启动任务 #" + t.ID + "：" + t.Description, true, nil
	default:
		return "未知快捷命令。发送 /help 查看可用命令，或直接用自然语言描述操作。", true, nil
	}
}

func (s *Server) runRemoteAutoConversation(conversationID int64, message string) (string, error) {
	c, err := s.m.pg.GetConversation(conversationID)
	if err != nil || c == nil {
		return "", errors.New("绑定的远程会话不存在，请解绑后重新配对")
	}
	ca := s.resolveChatAgent(c)
	if ca == nil {
		return "", errors.New(s.chatUnavailableReason())
	}
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	if s.chatBusy[busyKey] {
		s.chatMu.Unlock()
		return "", errors.New("上一条远程消息仍在处理中，请稍后再试")
	}
	ctx, cancel := context.WithCancelCause(intercept.WithConvID(s.ctx, c.ID))
	ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: message})
	s.chatBusy[busyKey] = true
	s.chatCancel[busyKey] = cancel
	s.chatMu.Unlock()
	defer func() {
		cancel(agent.AbortChatTurnFinished)
		s.chatMu.Lock()
		delete(s.chatBusy, busyKey)
		delete(s.chatCancel, busyKey)
		s.chatMu.Unlock()
	}()
	ua := userActivityWithAttachments(c.AgentKey, message, nil)
	ua.Summary, ua.Detail = firstLine(message, 200), message
	_, _ = s.m.pg.AppendConvActivity(c.ID, ua)
	maxTurns := s.agentMaxTurns(c.AgentKey)
	maxDuration := time.Duration(s.agentRunSeconds(c.AgentKey)) * time.Second
	webSearch := false
	if a, _ := s.m.pg.GetAgentByKey(c.AgentKey); a != nil {
		webSearch = a.WebSearch
	}
	emit := func(rec db.Activity) { _, _ = s.m.pg.AppendConvActivity(c.ID, rec) }
	reply, err := ca.Chat(ctx, c.AgentKey, busyKey, message, maxTurns, maxDuration, webSearch, emit)
	_ = s.m.pg.TouchConversation(c.ID)
	return strings.TrimSpace(reply), err
}

// ---- 微信 Claw / iLink adapter ----

type weChatBaseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent"`
}

type weChatMessageItem struct {
	Type     int `json:"type"`
	TextItem struct {
		Text string `json:"text"`
	} `json:"text_item"`
	VoiceItem struct {
		Text string `json:"text"`
	} `json:"voice_item"`
}

type weChatMessage struct {
	MessageID    json.RawMessage     `json:"message_id"`
	ClientID     string              `json:"client_id"`
	FromUserID   string              `json:"from_user_id"`
	SessionID    string              `json:"session_id"`
	GroupID      string              `json:"group_id"`
	MessageType  int                 `json:"message_type"`
	ItemList     []weChatMessageItem `json:"item_list"`
	ContextToken string              `json:"context_token"`
	RunID        string              `json:"run_id"`
}

type weChatUpdates struct {
	Ret                  int             `json:"ret"`
	ErrCode              int             `json:"errcode"`
	ErrMsg               string          `json:"errmsg"`
	Messages             []weChatMessage `json:"msgs"`
	GetUpdatesBuf        string          `json:"get_updates_buf"`
	LongPollingTimeoutMS int             `json:"longpolling_timeout_ms"`
}

type weChatQRStatus struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	ILinkBotID   string `json:"ilink_bot_id"`
	BaseURL      string `json:"baseurl"`
	ILinkUserID  string `json:"ilink_user_id"`
	RedirectHost string `json:"redirect_host"`
}

func weChatBase() weChatBaseInfo {
	version := strings.TrimSpace(BuildVersion)
	if version == "" {
		version = "dev"
	}
	return weChatBaseInfo{ChannelVersion: weChatChannelVersion, BotAgent: "ARTEX/" + version}
}

func weChatClientVersion(version string) string {
	var major, minor, patch uint64
	_, _ = fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
	return strconv.FormatUint((major&0xff)<<16|(minor&0xff)<<8|(patch&0xff), 10)
}

func weChatUIN() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint32(b[:], uint32(time.Now().UnixNano()))
	}
	decimal := strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)
	return base64.StdEncoding.EncodeToString([]byte(decimal))
}

func weChatBaseURL(cfg remoteChannelConfig) string {
	if base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"); base != "" {
		return base
	}
	return weChatDefaultBaseURL
}

func addWeChatAppHeaders(req *http.Request, cfg remoteChannelConfig) {
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", weChatClientVersion(weChatChannelVersion))
	if cfg.RouteTag != "" {
		req.Header.Set("SKRouteTag", cfg.RouteTag)
	}
}

func addWeChatJSONHeaders(req *http.Request, cfg remoteChannelConfig, authenticated bool) {
	addWeChatAppHeaders(req, cfg)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", weChatUIN())
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+cfg.BotToken)
	}
}

func doWeChatJSON(ctx context.Context, client *http.Client, method, endpoint string, payload any, cfg remoteChannelConfig, authenticated bool, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		addWeChatJSONHeaders(req, cfg, authenticated)
	} else {
		addWeChatAppHeaders(req, cfg)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("微信 Claw HTTP %d", resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil {
		return fmt.Errorf("解析微信 Claw 响应: %w", err)
	}
	return nil
}

func (s *Server) startWeChatLogin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad channel id")
		return
	}
	c, err := s.m.pg.GetRemoteChannel(id)
	if err != nil || c == nil || c.Kind != "wechat_claw" {
		writeErr(w, 404, "微信 Claw 渠道不存在")
		return
	}
	cfg := decodeRemoteConfig(c)
	var result struct {
		QRCode           string `json:"qrcode"`
		QRCodeImgContent string `json:"qrcode_img_content"`
	}
	client := &http.Client{Timeout: 15 * time.Second}
	endpoint := weChatDefaultBaseURL + "/ilink/bot/get_bot_qrcode?bot_type=3"
	localTokens := []string{}
	if cfg.BotToken != "" {
		localTokens = append(localTokens, cfg.BotToken)
	}
	if err := doWeChatJSON(r.Context(), client, http.MethodPost, endpoint, map[string]any{"local_token_list": localTokens}, cfg, false, &result); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if result.QRCode == "" || result.QRCodeImgContent == "" {
		writeErr(w, 502, "微信 Claw 未返回有效二维码")
		return
	}
	cfg.QRCode = result.QRCode
	cfg.QRBaseURL = weChatDefaultBaseURL
	updated, err := s.saveRemoteConfig(c, cfg)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "wait", "qrcode": result.QRCode, "qrcode_img_content": result.QRCodeImgContent, "channel": remoteChannelView(r, updated)})
}

func (s *Server) pollWeChatLogin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad channel id")
		return
	}
	var input struct {
		VerifyCode string `json:"verify_code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, remoteMaxBody)
	if err := decode(r, &input); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, err.Error())
		return
	}
	c, err := s.m.pg.GetRemoteChannel(id)
	if err != nil || c == nil || c.Kind != "wechat_claw" {
		writeErr(w, 404, "微信 Claw 渠道不存在")
		return
	}
	cfg := decodeRemoteConfig(c)
	if cfg.QRCode == "" {
		writeErr(w, 400, "请先获取微信登录二维码")
		return
	}
	base := cfg.QRBaseURL
	if base == "" {
		base = weChatBaseURL(cfg)
	}
	values := url.Values{"qrcode": []string{cfg.QRCode}}
	if code := strings.TrimSpace(input.VerifyCode); code != "" {
		values.Set("verify_code", code)
	}
	var status weChatQRStatus
	client := &http.Client{Timeout: weChatDefaultLongPoll + 5*time.Second}
	endpoint := strings.TrimRight(base, "/") + "/ilink/bot/get_qrcode_status?" + values.Encode()
	if err := doWeChatJSON(r.Context(), client, http.MethodGet, endpoint, nil, cfg, false, &status); err != nil {
		// The QR status endpoint is a long poll. Transient network/gateway
		// failures mean "still waiting", matching the official client behavior.
		writeJSON(w, 200, map[string]any{"status": "wait", "channel": remoteChannelView(r, c)})
		return
	}
	if status.RedirectHost != "" && status.Status == "scaned_but_redirect" {
		redirect := strings.TrimRight(status.RedirectHost, "/")
		if !strings.HasPrefix(redirect, "http://") && !strings.HasPrefix(redirect, "https://") {
			redirect = "https://" + redirect
		}
		cfg.QRBaseURL = redirect
	}
	if status.Status == "confirmed" {
		if status.BotToken == "" || status.ILinkBotID == "" {
			writeErr(w, 502, "微信 Claw 登录确认响应缺少凭据")
			return
		}
		cfg.BotToken = status.BotToken
		cfg.ILinkBotID = status.ILinkBotID
		cfg.ILinkUserID = status.ILinkUserID
		if status.BaseURL != "" {
			cfg.BaseURL = strings.TrimRight(status.BaseURL, "/")
		}
		cfg.QRCode, cfg.QRBaseURL = "", ""
	}
	updated, err := s.saveRemoteConfig(c, cfg)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status.Status == "confirmed" {
		s.restartWeChatChannel(updated)
	}
	writeJSON(w, 200, map[string]any{"status": status.Status, "channel": remoteChannelView(r, updated)})
}

func (s *Server) saveRemoteConfig(c *db.RemoteChannel, cfg remoteChannelConfig) (*db.RemoteChannel, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return s.m.pg.UpdateRemoteChannel(c.ID, c.Name, c.Enabled, encoded)
}

func (s *Server) startWeChatChannels() {
	channels, err := s.m.pg.ListRemoteChannels()
	if err != nil {
		log.Printf("[wechat-claw] load channels: %v", err)
		return
	}
	for _, c := range channels {
		s.restartWeChatChannel(c)
	}
}

func (s *Server) stopWeChatChannel(id int64) {
	s.remoteMu.Lock()
	cancel := s.remoteCancel[id]
	delete(s.remoteCancel, id)
	s.remoteMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) restartWeChatChannel(c *db.RemoteChannel) {
	if c == nil {
		return
	}
	s.stopWeChatChannel(c.ID)
	cfg := decodeRemoteConfig(c)
	if c.Kind != "wechat_claw" || !c.Enabled || cfg.BotToken == "" {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.remoteMu.Lock()
	s.remoteCancel[c.ID] = cancel
	s.remoteMu.Unlock()
	go s.runWeChatChannel(ctx, c, cfg)
}

func (s *Server) runWeChatChannel(ctx context.Context, c *db.RemoteChannel, cfg remoteChannelConfig) {
	log.Printf("[wechat-claw] channel %d started (bot=%s)", c.ID, cfg.ILinkBotID)
	_ = notifyWeChat(ctx, cfg, "notifystart")
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = notifyWeChat(stopCtx, cfg, "notifystop")
		log.Printf("[wechat-claw] channel %d stopped", c.ID)
	}()
	cursor := ""
	backoff := time.Second
	for {
		updates, err := getWeChatUpdates(ctx, cfg, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[wechat-claw] channel %d poll: %v", c.ID, err)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if updates.GetUpdatesBuf != "" {
			cursor = updates.GetUpdatesBuf
		}
		for i := range updates.Messages {
			msg := updates.Messages[i]
			go s.handleWeChatMessage(c, cfg, msg)
		}
	}
}

func getWeChatUpdates(ctx context.Context, cfg remoteChannelConfig, cursor string) (*weChatUpdates, error) {
	client := &http.Client{Timeout: weChatDefaultLongPoll + 10*time.Second}
	payload := map[string]any{"get_updates_buf": cursor, "base_info": weChatBase()}
	var result weChatUpdates
	err := doWeChatJSON(ctx, client, http.MethodPost, weChatBaseURL(cfg)+"/ilink/bot/getupdates", payload, cfg, true, &result)
	if err != nil {
		return nil, err
	}
	if result.Ret != 0 || result.ErrCode != 0 {
		return nil, fmt.Errorf("ret=%d errcode=%d %s", result.Ret, result.ErrCode, result.ErrMsg)
	}
	return &result, nil
}

func notifyWeChat(ctx context.Context, cfg remoteChannelConfig, action string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	var result struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	endpoint := weChatBaseURL(cfg) + "/ilink/bot/msg/" + action
	err := doWeChatJSON(ctx, client, http.MethodPost, endpoint, map[string]any{"base_info": weChatBase()}, cfg, true, &result)
	if err == nil && result.Ret != 0 {
		err = fmt.Errorf("ret=%d %s", result.Ret, result.ErrMsg)
	}
	return err
}

func weChatMessageText(msg weChatMessage) string {
	parts := make([]string, 0, len(msg.ItemList))
	for _, item := range msg.ItemList {
		var text string
		switch item.Type {
		case 1:
			text = item.TextItem.Text
		case 3:
			text = item.VoiceItem.Text
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func weChatExternalMessageID(msg weChatMessage) string {
	if id := strings.Trim(strings.TrimSpace(string(msg.MessageID)), `"`); id != "" && id != "null" {
		return id
	}
	return msg.ClientID
}

func (s *Server) handleWeChatMessage(c *db.RemoteChannel, cfg remoteChannelConfig, msg weChatMessage) {
	if msg.MessageType != 1 || strings.TrimSpace(msg.FromUserID) == "" {
		return
	}
	text := weChatMessageText(msg)
	messageID := weChatExternalMessageID(msg)
	if text == "" || messageID == "" {
		return
	}
	chatID := strings.TrimSpace(msg.GroupID)
	if chatID == "" {
		chatID = strings.TrimSpace(msg.SessionID)
	}
	if chatID == "" {
		chatID = msg.FromUserID
	}
	in := remoteInbound{MessageID: messageID, SenderID: msg.FromUserID, ChatID: chatID, Text: text}
	job, created, err := s.enqueueRemoteMessage(c, in)
	if err != nil || !created {
		if err != nil {
			log.Printf("[wechat-claw] enqueue: %v", err)
		}
		return
	}
	s.processRemoteMessage(c, job, in, func(reply string) error {
		return sendWeChatReply(s.ctx, cfg, msg.FromUserID, msg.ContextToken, msg.RunID, reply)
	})
}

func sendWeChatReply(ctx context.Context, cfg remoteChannelConfig, toUserID, contextToken, runID, text string) error {
	if len([]rune(text)) > weChatMaximumReplyRuneCount {
		text = string([]rune(text)[:weChatMaximumReplyRuneCount]) + "\n…（回复过长，已截断）"
	}
	clientID, err := randomRemoteSecret(18)
	if err != nil {
		return err
	}
	msg := map[string]any{
		"from_user_id": "", "to_user_id": toUserID, "client_id": clientID,
		"message_type": 2, "message_state": 2, "context_token": contextToken,
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": text}}},
	}
	if runID != "" {
		msg["run_id"] = runID
	}
	payload := map[string]any{"msg": msg, "base_info": weChatBase()}
	var result struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if err := doWeChatJSON(ctx, client, http.MethodPost, weChatBaseURL(cfg)+"/ilink/bot/sendmessage", payload, cfg, true, &result); err != nil {
		return err
	}
	if result.Ret != 0 {
		return fmt.Errorf("微信 Claw 回复失败: ret=%d %s", result.Ret, result.ErrMsg)
	}
	return nil
}

// ---- Feishu adapter ----

type feishuEventEnvelope struct {
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Type      string `json:"type"`
	Schema    string `json:"schema"`
	Header    struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Token     string `json:"token"`
	} `json:"header"`
	Event struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
				UserID string `json:"user_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
	} `json:"event"`
}

func verifyFeishuSignature(r *http.Request, raw []byte, encryptKey string) bool {
	if encryptKey == "" {
		return true
	}
	timestamp, nonce, sig := r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), r.Header.Get("X-Lark-Signature")
	if timestamp == "" || nonce == "" || sig == "" {
		return false
	}
	h := sha256.Sum256(append([]byte(timestamp+nonce+encryptKey), raw...))
	want := hex.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sig)), []byte(want)) == 1
}

// decryptFeishuEvent mirrors the documented Feishu SDK algorithm: the decoded
// payload starts with a 16-byte IV, followed by AES-256-CBC ciphertext. The SDK
// extracts the JSON object from the decrypted/padded bytes.
func decryptFeishuEvent(encrypted, encryptKey string) ([]byte, error) {
	buf, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(buf) < aes.BlockSize*2 {
		return nil, errors.New("invalid encrypted feishu event")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	iv, ciphertext := buf[:aes.BlockSize], buf[aes.BlockSize:]
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("invalid feishu ciphertext size")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(ciphertext, ciphertext)
	start, end := bytes.IndexByte(ciphertext, '{'), bytes.LastIndexByte(ciphertext, '}')
	if start < 0 || end < start {
		return nil, errors.New("decrypted feishu event has no json")
	}
	return ciphertext[start : end+1], nil
}

func (s *Server) feishuHook(w http.ResponseWriter, r *http.Request) {
	c, err := s.m.pg.GetRemoteChannelByEndpoint(r.PathValue("endpoint"))
	if err != nil || c == nil || !c.Enabled || c.Kind != "feishu" {
		writeErr(w, 404, "remote channel not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, remoteMaxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "bad body")
		return
	}
	cfg := decodeRemoteConfig(c)
	if !verifyFeishuSignature(r, raw, cfg.EncryptKey) {
		writeErr(w, 401, "invalid feishu signature")
		return
	}
	plain := raw
	var encrypted struct {
		Encrypt string `json:"encrypt"`
	}
	if json.Unmarshal(raw, &encrypted) == nil && encrypted.Encrypt != "" {
		if cfg.EncryptKey == "" {
			writeErr(w, 400, "encrypted feishu event requires encrypt_key")
			return
		}
		plain, err = decryptFeishuEvent(encrypted.Encrypt, cfg.EncryptKey)
		if err != nil {
			writeErr(w, 400, "cannot decrypt feishu event")
			return
		}
	}
	var env feishuEventEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	providedToken := env.Header.Token
	if providedToken == "" {
		providedToken = env.Token
	}
	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(cfg.VerificationToken)) != 1 {
		writeErr(w, 401, "invalid feishu verification token")
		return
	}
	if env.Challenge != "" || env.Type == "url_verification" {
		writeJSON(w, 200, map[string]string{"challenge": env.Challenge})
		return
	}
	// Acknowledge every valid event immediately; Feishu retries slow callbacks.
	writeJSON(w, 200, map[string]any{"code": 0})
	if env.Header.EventType != "im.message.receive_v1" || env.Event.Sender.SenderType == "app" || env.Event.Message.MessageType != "text" {
		return
	}
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(env.Event.Message.Content), &content) != nil || strings.TrimSpace(content.Text) == "" {
		return
	}
	sender := env.Event.Sender.SenderID.OpenID
	if sender == "" {
		sender = env.Event.Sender.SenderID.UserID
	}
	in := remoteInbound{MessageID: env.Header.EventID, SenderID: sender, ChatID: env.Event.Message.ChatID, Text: strings.TrimSpace(content.Text)}
	job, created, err := s.enqueueRemoteMessage(c, in)
	if err != nil || !created {
		return
	}
	go s.processRemoteMessage(c, job, in, func(reply string) error {
		return sendFeishuReply(context.Background(), cfg, env.Event.Message.MessageID, reply)
	})
}

func sendFeishuReply(ctx context.Context, cfg remoteChannelConfig, messageID, text string) error {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://open.feishu.cn"
	}
	client := &http.Client{Timeout: 15 * time.Second}
	body, _ := json.Marshal(map[string]string{"app_id": cfg.AppID, "app_secret": cfg.AppSecret})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tokenResp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResp); err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 || tokenResp.Code != 0 || tokenResp.TenantAccessToken == "" {
		return fmt.Errorf("feishu token: code=%d %s", tokenResp.Code, tokenResp.Msg)
	}
	if len([]rune(text)) > 12000 {
		text = string([]rune(text)[:12000]) + "\n…（回复过长，已截断）"
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	payload, _ := json.Marshal(map[string]string{"msg_type": "text", "content": string(content)})
	replyURL := base + "/open-apis/im/v1/messages/" + url.PathEscape(messageID) + "/reply"
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, replyURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tokenResp.TenantAccessToken)
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if resp.StatusCode/100 != 2 || result.Code != 0 {
		return fmt.Errorf("feishu reply: http=%d code=%d %s", resp.StatusCode, result.Code, result.Msg)
	}
	return nil
}
