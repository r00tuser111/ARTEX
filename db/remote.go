package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// RemoteChannel is one externally reachable chat adapter. Config contains the
// provider credentials needed at runtime; API DTOs must redact it before return.
type RemoteChannel struct {
	ID          int64           `json:"id"`
	EndpointKey string          `json:"endpoint_key"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Config      json.RawMessage `json:"-"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type RemoteBinding struct {
	ID             int64     `json:"id"`
	ChannelID      int64     `json:"channel_id"`
	ExternalUserID string    `json:"external_user_id"`
	ExternalChatID string    `json:"external_chat_id"`
	DisplayName    string    `json:"display_name"`
	ConversationID int64     `json:"conversation_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type RemoteMessage struct {
	ID                string    `json:"id"`
	ChannelID         int64     `json:"channel_id"`
	ExternalMessageID string    `json:"external_message_id"`
	ExternalUserID    string    `json:"external_user_id"`
	ExternalChatID    string    `json:"external_chat_id"`
	ConversationID    *int64    `json:"conversation_id,omitempty"`
	Status            string    `json:"status"`
	RequestText       string    `json:"request_text,omitempty"`
	ReplyText         string    `json:"reply,omitempty"`
	ErrorText         string    `json:"error,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const remoteChannelCols = `id,endpoint_key,kind,name,enabled,config,created_at,updated_at`

func scanRemoteChannel(row interface{ Scan(...any) error }) (*RemoteChannel, error) {
	var c RemoteChannel
	err := row.Scan(&c.ID, &c.EndpointKey, &c.Kind, &c.Name, &c.Enabled, &c.Config, &c.CreatedAt, &c.UpdatedAt)
	return &c, err
}

func (d *DB) CreateRemoteChannel(endpointKey, kind, name string, enabled bool, config json.RawMessage) (*RemoteChannel, error) {
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	return scanRemoteChannel(d.QueryRow(`
INSERT INTO remote_channels(endpoint_key,kind,name,enabled,config)
VALUES ($1,$2,$3,$4,$5) RETURNING `+remoteChannelCols, endpointKey, kind, name, enabled, config))
}

func (d *DB) ListRemoteChannels() ([]*RemoteChannel, error) {
	rows, err := d.Query(`SELECT ` + remoteChannelCols + ` FROM remote_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RemoteChannel{}
	for rows.Next() {
		c, err := scanRemoteChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) GetRemoteChannelByEndpoint(endpointKey string) (*RemoteChannel, error) {
	c, err := scanRemoteChannel(d.QueryRow(`SELECT `+remoteChannelCols+` FROM remote_channels WHERE endpoint_key=$1`, endpointKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (d *DB) GetRemoteChannel(id int64) (*RemoteChannel, error) {
	c, err := scanRemoteChannel(d.QueryRow(`SELECT `+remoteChannelCols+` FROM remote_channels WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (d *DB) UpdateRemoteChannel(id int64, name string, enabled bool, config json.RawMessage) (*RemoteChannel, error) {
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	c, err := scanRemoteChannel(d.QueryRow(`UPDATE remote_channels SET
name=$2,enabled=$3,config=$4
WHERE id=$1 RETURNING `+remoteChannelCols, id, name, enabled, config))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (d *DB) DeleteRemoteChannel(id int64) error {
	_, err := d.Exec(`DELETE FROM remote_channels WHERE id=$1`, id)
	return err
}

func (d *DB) CreateRemotePairingCode(channelID int64, codeHash string, expires time.Time) error {
	_, err := d.Exec(`WITH invalidated AS (
  UPDATE remote_pairing_codes SET used_at=now()
  WHERE channel_id=$1 AND used_at IS NULL
)
INSERT INTO remote_pairing_codes(channel_id,code_hash,expires_at) VALUES ($1,$2,$3)`, channelID, codeHash, expires)
	return err
}

// ConsumeRemotePairingCode atomically marks one live code used.
func (d *DB) ConsumeRemotePairingCode(channelID int64, codeHash string) (bool, error) {
	var id int64
	err := d.QueryRow(`UPDATE remote_pairing_codes SET used_at=now() WHERE id=(
  SELECT id FROM remote_pairing_codes
  WHERE channel_id=$1 AND code_hash=$2 AND used_at IS NULL AND expires_at>now()
  ORDER BY id DESC LIMIT 1 FOR UPDATE SKIP LOCKED
) RETURNING id`, channelID, codeHash).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (d *DB) GetRemoteBinding(channelID int64, userID, chatID string) (*RemoteBinding, error) {
	var b RemoteBinding
	err := d.QueryRow(`SELECT id,channel_id,external_user_id,external_chat_id,display_name,conversation_id,created_at,updated_at
FROM remote_bindings WHERE channel_id=$1 AND external_user_id=$2 AND external_chat_id=$3`, channelID, userID, chatID).
		Scan(&b.ID, &b.ChannelID, &b.ExternalUserID, &b.ExternalChatID, &b.DisplayName, &b.ConversationID, &b.CreatedAt, &b.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

func (d *DB) CreateRemoteBinding(channelID int64, userID, chatID, displayName string, conversationID int64) (*RemoteBinding, error) {
	var b RemoteBinding
	err := d.QueryRow(`INSERT INTO remote_bindings(channel_id,external_user_id,external_chat_id,display_name,conversation_id)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT(channel_id,external_user_id,external_chat_id) DO UPDATE
SET display_name=EXCLUDED.display_name
RETURNING id,channel_id,external_user_id,external_chat_id,display_name,conversation_id,created_at,updated_at`,
		channelID, userID, chatID, displayName, conversationID).
		Scan(&b.ID, &b.ChannelID, &b.ExternalUserID, &b.ExternalChatID, &b.DisplayName, &b.ConversationID, &b.CreatedAt, &b.UpdatedAt)
	return &b, err
}

func (d *DB) ListRemoteBindings(channelID int64) ([]*RemoteBinding, error) {
	rows, err := d.Query(`SELECT id,channel_id,external_user_id,external_chat_id,display_name,conversation_id,created_at,updated_at
FROM remote_bindings WHERE ($1=0 OR channel_id=$1) ORDER BY updated_at DESC`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RemoteBinding{}
	for rows.Next() {
		var b RemoteBinding
		if err := rows.Scan(&b.ID, &b.ChannelID, &b.ExternalUserID, &b.ExternalChatID, &b.DisplayName, &b.ConversationID, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

func (d *DB) DeleteRemoteBinding(id int64) error {
	_, err := d.Exec(`DELETE FROM remote_bindings WHERE id=$1`, id)
	return err
}

const remoteMessageCols = `id,channel_id,external_message_id,external_user_id,external_chat_id,conversation_id,status,request_text,reply_text,error_text,created_at,updated_at`

func scanRemoteMessage(row interface{ Scan(...any) error }) (*RemoteMessage, error) {
	var m RemoteMessage
	err := row.Scan(&m.ID, &m.ChannelID, &m.ExternalMessageID, &m.ExternalUserID, &m.ExternalChatID,
		&m.ConversationID, &m.Status, &m.RequestText, &m.ReplyText, &m.ErrorText, &m.CreatedAt, &m.UpdatedAt)
	return &m, err
}

// CreateRemoteMessage is idempotent on the provider's external message id.
func (d *DB) CreateRemoteMessage(m *RemoteMessage) (*RemoteMessage, bool, error) {
	created := true
	row, err := scanRemoteMessage(d.QueryRow(`INSERT INTO remote_messages
(id,channel_id,external_message_id,external_user_id,external_chat_id,conversation_id,status,request_text)
VALUES ($1,$2,$3,$4,$5,$6,'queued',$7)
ON CONFLICT(channel_id,external_message_id) DO NOTHING RETURNING `+remoteMessageCols,
		m.ID, m.ChannelID, m.ExternalMessageID, m.ExternalUserID, m.ExternalChatID, m.ConversationID, m.RequestText))
	if err == sql.ErrNoRows {
		created = false
		row, err = scanRemoteMessage(d.QueryRow(`SELECT `+remoteMessageCols+` FROM remote_messages WHERE channel_id=$1 AND external_message_id=$2`, m.ChannelID, m.ExternalMessageID))
	}
	return row, created, err
}

func (d *DB) GetRemoteMessage(id string) (*RemoteMessage, error) {
	m, err := scanRemoteMessage(d.QueryRow(`SELECT `+remoteMessageCols+` FROM remote_messages WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (d *DB) StartRemoteMessage(id string, conversationID int64) error {
	_, err := d.Exec(`UPDATE remote_messages SET status='running',conversation_id=$2 WHERE id=$1 AND status='queued'`, id, conversationID)
	return err
}

func (d *DB) FinishRemoteMessage(id, status, reply, errorText string) error {
	_, err := d.Exec(`UPDATE remote_messages SET status=$2,reply_text=$3,error_text=$4 WHERE id=$1`, id, status, reply, errorText)
	return err
}
