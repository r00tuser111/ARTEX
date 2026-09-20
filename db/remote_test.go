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

	if err := d.CreateRemotePairingCode(channel.ID, "old-code-hash", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateRemotePairingCode(channel.ID, "code-hash", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ok, err := d.ConsumeRemotePairingCode(channel.ID, "old-code-hash")
	if err != nil || ok {
		t.Fatalf("replaced pairing consume=(%v,%v), want false,nil", ok, err)
	}
	ok, err = d.ConsumeRemotePairingCode(channel.ID, "code-hash")
	if err != nil || !ok {
		t.Fatalf("first pairing consume=(%v,%v)", ok, err)
	}
	ok, err = d.ConsumeRemotePairingCode(channel.ID, "code-hash")
	if err != nil || ok {
		t.Fatalf("second pairing consume=(%v,%v), want false,nil", ok, err)
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
