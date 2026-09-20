package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePairCommand(t *testing.T) {
	for _, tc := range []struct {
		input string
		code  string
		ok    bool
	}{{"/bind 123456", "123456", true}, {" 绑定 abcdef ", "abcdef", true}, {"/bind", "", false}, {"hello", "", false}} {
		got, ok := parsePairCommand(tc.input)
		if got != tc.code || ok != tc.ok {
			t.Fatalf("parsePairCommand(%q)=(%q,%v), want (%q,%v)", tc.input, got, ok, tc.code, tc.ok)
		}
	}
}

func TestRemotePairingHashUsesServerKey(t *testing.T) {
	a := (&Server{jwtKey: []byte("key-a")}).remotePairingHash("123456")
	b := (&Server{jwtKey: []byte("key-b")}).remotePairingHash("123456")
	if a == b || a == remoteSecretHash("123456") {
		t.Fatal("pairing code must be keyed by the server secret")
	}
}

func TestVerifyFeishuSignature(t *testing.T) {
	body := []byte(`{"schema":"2.0"}`)
	key := "encrypt-key"
	timestamp, nonce := "1700000000", "nonce"
	sum := sha256.Sum256(append([]byte(timestamp+nonce+key), body...))
	req := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", timestamp)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", remoteSecretHash(string(append([]byte(timestamp+nonce+key), body...))))
	// remoteSecretHash is the same SHA-256 hex operation used by Feishu.
	if !verifyFeishuSignature(req, body, key) {
		t.Fatal("valid signature rejected")
	}
	req.Header.Set("X-Lark-Signature", strings.Repeat("0", len(sum)*2))
	if verifyFeishuSignature(req, body, key) {
		t.Fatal("invalid signature accepted")
	}
}

func TestDecryptFeishuEvent(t *testing.T) {
	plain := []byte(`{"challenge":"ok","token":"v"}`)
	secret := "event-encrypt-key"
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	iv := []byte("0123456789abcdef")
	// Feishu's decoder extracts the JSON braces, so zero padding is sufficient
	// for this deterministic fixture.
	padded := append([]byte(nil), plain...)
	for len(padded)%aes.BlockSize != 0 {
		padded = append(padded, 0)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(padded, padded)
	encrypted := base64.StdEncoding.EncodeToString(append(iv, padded...))
	got, err := decryptFeishuEvent(encrypted, secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("decrypt=%q want %q", got, plain)
	}
}

func TestWeChatClientVersion(t *testing.T) {
	if got := weChatClientVersion("2.4.9"); got != "132105" {
		t.Fatalf("version=%s want 132105", got)
	}
}

func TestWeChatMessageText(t *testing.T) {
	var msg weChatMessage
	if err := json.Unmarshal([]byte(`{
      "message_id":123,"message_type":1,"item_list":[
        {"type":1,"text_item":{"text":" /tasks "}},
        {"type":3,"voice_item":{"text":"查看任务状态"}}
      ]}`), &msg); err != nil {
		t.Fatal(err)
	}
	if got := weChatExternalMessageID(msg); got != "123" {
		t.Fatalf("message id=%q", got)
	}
	if got := weChatMessageText(msg); got != "/tasks\n查看任务状态" {
		t.Fatalf("text=%q", got)
	}
}

func TestWeChatHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	addWeChatJSONHeaders(req, remoteChannelConfig{BotToken: "secret", RouteTag: "route"}, true)
	if req.Header.Get("Authorization") != "Bearer secret" || req.Header.Get("AuthorizationType") != "ilink_bot_token" {
		t.Fatal("missing iLink authorization headers")
	}
	if req.Header.Get("iLink-App-Id") != "bot" || req.Header.Get("iLink-App-ClientVersion") != "132105" {
		t.Fatal("missing iLink application headers")
	}
	if _, err := base64.StdEncoding.DecodeString(req.Header.Get("X-WECHAT-UIN")); err != nil {
		t.Fatalf("bad X-WECHAT-UIN: %v", err)
	}
}

func TestWeChatGetUpdatesAndReply(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ilink/bot/getupdates", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"get_updates_buf":"cursor-1"`) || !strings.Contains(string(body), `"bot_agent":"ARTEX/`) {
			t.Errorf("getupdates body=%s", body)
		}
		_, _ = w.Write([]byte(`{"ret":0,"msgs":[],"get_updates_buf":"cursor-2"}`))
	})
	mux.HandleFunc("POST /ilink/bot/sendmessage", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Message struct {
				ToUserID     string `json:"to_user_id"`
				ContextToken string `json:"context_token"`
				RunID        string `json:"run_id"`
			} `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode reply: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Message.ToUserID != "user-1" || payload.Message.ContextToken != "ctx-1" || payload.Message.RunID != "run-1" {
			t.Errorf("reply payload=%+v", payload.Message)
		}
		_, _ = w.Write([]byte(`{"ret":0}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	cfg := remoteChannelConfig{BaseURL: server.URL, BotToken: "bot-token"}
	updates, err := getWeChatUpdates(context.Background(), cfg, "cursor-1")
	if err != nil || updates.GetUpdatesBuf != "cursor-2" {
		t.Fatalf("get updates: result=%+v err=%v", updates, err)
	}
	if err := sendWeChatReply(context.Background(), cfg, "user-1", "ctx-1", "run-1", "ok"); err != nil {
		t.Fatal(err)
	}
}
