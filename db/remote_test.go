package db

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestRemoteChannelPairingBindingAndMessageDedup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	suffix := time.Now().UnixNano()
	channel, err := d.CreateRemoteChannel(fmt.Sprintf("ep-%d", suffix), "wechat_claw", "test remote", true, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var convID int64
	t.Cleanup(func() {
		_ = d.DeleteRemoteChannel(channel.ID)
		if convID > 0 {
			_ = d.DeleteConversation(convID)
		}
	})

	request, created, err := d.GetOrCreateRemotePairingRequest(channel.ID, "pair-user", "pair-chat", "pair tester",
		"ABCDEFGH", "code-hash", time.Now().Add(time.Hour))
	if err != nil || !created || request.Code != "ABCDEFGH" {
		t.Fatalf("pairing request=%+v created=%v err=%v", request, created, err)
	}
	repeated, created, err := d.GetOrCreateRemotePairingRequest(channel.ID, "pair-user", "pair-chat", "pair tester",
		"ZZZZZZZZ", "other-hash", time.Now().Add(time.Hour))
	if err != nil || created || repeated.ID != request.ID || repeated.Code != request.Code {
		t.Fatalf("repeated pairing request=%+v created=%v err=%v", repeated, created, err)
	}
	requests, err := d.ListRemotePairingRequests(channel.ID)
	if err != nil || len(requests) != 1 || requests[0].ID != request.ID {
		t.Fatalf("pairing requests=%+v err=%v", requests, err)
	}
	if err := d.ResolveRemotePairingRequest(request.ID); err != nil {
		t.Fatal(err)
	}
	requests, err = d.ListRemotePairingRequests(channel.ID)
	if err != nil || len(requests) != 0 {
		t.Fatalf("resolved pairing requests=%+v err=%v", requests, err)
	}

	conv, err := d.CreateConversation("auto", "remote-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	convID = conv.ID
	binding, err := d.CreateRemoteBinding(channel.ID, "user", "chat", "tester", conv.ID)
	if err != nil || binding.ConversationID != conv.ID {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	qrBinding, err := d.CreateRemoteBinding(channel.ID, "qr-user", "", "scanner", conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolvedQRBinding, err := d.GetRemoteBinding(channel.ID, "qr-user", "dynamic-session-id")
	if err != nil || resolvedQRBinding == nil || resolvedQRBinding.ID != qrBinding.ID {
		t.Fatalf("QR binding fallback=%+v err=%v", resolvedQRBinding, err)
	}

	input := &RemoteMessage{ID: fmt.Sprintf("job-%d", suffix), ChannelID: channel.ID, ExternalMessageID: "event-1",
		ExternalUserID: "user", ExternalChatID: "chat", ConversationID: &conv.ID, RequestText: "/tasks"}
	first, created, err := d.CreateRemoteMessage(input)
	if err != nil || !created {
		t.Fatalf("first message=%+v created=%v err=%v", first, created, err)
	}
	duplicate := *input
	duplicate.ID += "-duplicate"
	second, created, err := d.CreateRemoteMessage(&duplicate)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("duplicate=%+v created=%v err=%v", second, created, err)
	}
	if err := d.StartRemoteMessage(first.ID, conv.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishRemoteMessage(first.ID, "completed", "ok", ""); err != nil {
		t.Fatal(err)
	}
	finished, err := d.GetRemoteMessage(first.ID)
	if err != nil || finished.Status != "completed" || finished.ReplyText != "ok" {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
}
