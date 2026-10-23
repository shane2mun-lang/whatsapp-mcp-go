package main

// Native group history sharing: give members who were just added the recent
// group messages, the way WhatsApp's own apps do when an admin turns on "send
// recent messages". The shape below follows a real share captured on
// 2026-10-23 (phone -> linked device): a zlib-compressed GroupHistory uploaded
// as "Group History" media, a messageHistoryBundle sent group-direct to the new
// members, then a messageHistoryNotice to the whole group.
//
// Not wired to any endpoint yet: /api/group/share-history still answers 410
// until a throwaway-number test passes. The send itself lives in
// grouphistory_send.go behind the "grouphistory" build tag, because it needs
// the whatsmeow additions in wa-manager-go's patches/whatsmeow.patch
// (MediaGroupHistory, SendRequestExtra.GroupHistoryReceivers).

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"go.mau.fi/util/random"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waGroupHistory"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	// WhatsApp caps a share at 100 messages.
	groupHistoryMaxMessages = 100
	// WhatsApp's help centre documents 14 days. A phone share was seen reaching
	// further back, so 14 days stays well inside what WhatsApp sends itself.
	groupHistoryWindow = 14 * 24 * time.Hour
	// The bundle body is a protobuf; this is the mimetype the phone sets.
	groupHistoryMimetype = "application/protobuf"
)

// groupHistoryRow is one stored group message that may be shared.
type groupHistoryRow struct {
	ID        string
	Sender    types.JID // resolved sender; empty for own messages
	Content   string
	Time      time.Time
	IsFromMe  bool
	MediaType string
	Deleted   bool
	Failed    bool
	Secret    []byte // messageSecret from whatsmeow's store, when known
}

// selectGroupHistoryRows keeps text messages inside the share window and
// returns at most max of them, newest first (the order the phone uses).
// Media is left out until media sharing is built.
func selectGroupHistoryRows(rows []groupHistoryRow, now time.Time, window time.Duration, max int) []groupHistoryRow {
	since := now.Add(-window)
	seen := make(map[string]bool, len(rows))
	out := make([]groupHistoryRow, 0, len(rows))
	for _, r := range rows {
		switch {
		case r.ID == "" || seen[r.ID]:
			continue
		case r.Content == "" || r.MediaType != "":
			continue
		case r.Deleted || r.Failed:
			continue
		case r.Time.Before(since) || r.Time.After(now):
			continue
		case !r.IsFromMe && r.Sender.IsEmpty():
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// buildGroupHistory maps selected rows to the GroupHistory body. Field by
// field it mirrors the captured phone share: own messages carry
// originalSelfAuthorUserJIDString and no secret; others carry the sender as
// participant (key and message) and their messageSecret when it is known;
// every entry has interactiveMessageAdditionalMetadata; status stays unset.
func buildGroupHistory(group, ownLID types.JID, rows []groupHistoryRow) *waGroupHistory.GroupHistory {
	gh := &waGroupHistory.GroupHistory{Messages: make([]*waWeb.WebMessageInfo, 0, len(rows))}
	for _, r := range rows {
		key := &waCommon.MessageKey{
			RemoteJID: proto.String(group.String()),
			FromMe:    proto.Bool(r.IsFromMe),
			ID:        proto.String(r.ID),
		}
		msg := &waE2E.Message{Conversation: proto.String(r.Content)}
		wmi := &waWeb.WebMessageInfo{
			Key:              key,
			Message:          msg,
			MessageTimestamp: proto.Uint64(uint64(r.Time.Unix())),
			InteractiveMessageAdditionalMetadata: &waWeb.InteractiveMessageAdditionalMetadata{
				IsGalaxyFlowCompleted: proto.Bool(false),
			},
		}
		if r.IsFromMe {
			wmi.OriginalSelfAuthorUserJIDString = proto.String(ownLID.ToNonAD().String())
		} else {
			sender := r.Sender.ToNonAD().String()
			key.Participant = proto.String(sender)
			wmi.Participant = proto.String(sender)
			if len(r.Secret) > 0 {
				wmi.MessageSecret = r.Secret
				msg.MessageContextInfo = &waE2E.MessageContextInfo{MessageSecret: r.Secret}
			}
		}
		gh.Messages = append(gh.Messages, wmi)
	}
	return gh
}

// packGroupHistory serializes and zlib-compresses the body for upload.
func packGroupHistory(gh *waGroupHistory.GroupHistory) ([]byte, error) {
	raw, err := proto.Marshal(gh)
	if err != nil {
		return nil, fmt.Errorf("marshal group history: %w", err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("compress group history: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compress group history: %w", err)
	}
	return buf.Bytes(), nil
}

// groupHistoryMetadata matches the phone: the receivers, the message count
// and, as oldestMessageTimestampInWindow, the oldest shared message's time.
func groupHistoryMetadata(receivers []types.JID, rows []groupHistoryRow) *waE2E.MessageHistoryMetadata {
	jids := make([]string, len(receivers))
	for i, r := range receivers {
		jids[i] = r.ToNonAD().String()
	}
	oldest := rows[0].Time
	for _, r := range rows {
		if r.Time.Before(oldest) {
			oldest = r.Time
		}
	}
	return &waE2E.MessageHistoryMetadata{
		HistoryReceivers:               jids,
		OldestMessageTimestampInWindow: proto.Int64(oldest.Unix()),
		MessageCount:                   proto.Int64(int64(len(rows))),
	}
}

func buildGroupHistoryBundle(up whatsmeow.UploadResponse, meta *waE2E.MessageHistoryMetadata) *waE2E.Message {
	return &waE2E.Message{
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: random.Bytes(32)},
		MessageHistoryBundle: &waE2E.MessageHistoryBundle{
			Mimetype:          proto.String(groupHistoryMimetype),
			FileSHA256:        up.FileSHA256,
			MediaKey:          up.MediaKey,
			FileEncSHA256:     up.FileEncSHA256,
			DirectPath:        proto.String(up.DirectPath),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
			// The phone marks the bundle's key as an end-to-end media key.
			ContextInfo: &waE2E.ContextInfo{
				MediaDomainInfo: &waE2E.MediaDomainInfo{
					MediaKeyDomain: waE2E.MediaKeyDomain_MEDIA_KEY_DOMAIN_E2EE.Enum(),
					E2EeMediaKey:   up.MediaKey,
				},
			},
			MessageHistoryMetadata: meta,
		},
	}
}

func buildGroupHistoryNotice(meta *waE2E.MessageHistoryMetadata) *waE2E.Message {
	return &waE2E.Message{
		MessageContextInfo:   &waE2E.MessageContextInfo{MessageSecret: random.Bytes(32)},
		MessageHistoryNotice: &waE2E.MessageHistoryNotice{MessageHistoryMetadata: meta},
	}
}

// loadGroupHistoryRows reads recent stored messages of a group, newest first.
func (store *MessageStore) loadGroupHistoryRows(chatJID string, since time.Time, limit int) ([]groupHistoryRow, error) {
	q := `SELECT id, sender, content, timestamp, is_from_me, media_type, deleted_at IS NOT NULL, failed_at IS NOT NULL
		FROM messages WHERE chat_jid = ? AND timestamp >= ? ORDER BY timestamp DESC LIMIT ?`
	if isPostgres {
		q = `SELECT id, sender, content, timestamp, is_from_me, media_type, deleted_at IS NOT NULL, failed_at IS NOT NULL
		FROM messages WHERE chat_jid = $1 AND timestamp >= $2 ORDER BY timestamp DESC LIMIT $3`
	}
	rows, err := store.db.Query(q, chatJID, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []groupHistoryRow
	for rows.Next() {
		var r groupHistoryRow
		var sender, content, mediaType sql.NullString
		var senderUser string
		if err := rows.Scan(&r.ID, &sender, &content, &r.Time, &r.IsFromMe, &mediaType, &r.Deleted, &r.Failed); err != nil {
			return nil, err
		}
		senderUser = sender.String
		r.Content = content.String
		r.MediaType = mediaType.String
		if senderUser != "" {
			// The store keeps only the user part; resolveGroupHistorySenders
			// turns it into the JID the group addresses members by.
			r.Sender = types.JID{User: senderUser}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveGroupHistorySenders turns stored sender user parts into group
// identities (LID in LID-addressed groups) and attaches known message
// secrets. A sender that cannot be resolved is cleared, so selection skips it.
func resolveGroupHistorySenders(ctx context.Context, client *whatsmeow.Client, group types.JID, mode types.AddressingMode, rows []groupHistoryRow) {
	for i := range rows {
		r := &rows[i]
		if r.IsFromMe || r.Sender.User == "" {
			r.Sender = types.EmptyJID
			continue
		}
		pn := types.NewJID(r.Sender.User, types.DefaultUserServer)
		lid, _ := client.Store.LIDs.GetLIDForPN(ctx, pn)
		if lid.IsEmpty() {
			// Stored user part may already be a LID (no PN mapping known).
			asLID := types.NewJID(r.Sender.User, types.HiddenUserServer)
			if mapped, _ := client.Store.LIDs.GetPNForLID(ctx, asLID); !mapped.IsEmpty() {
				lid, pn = asLID, mapped
			}
		}
		switch {
		case mode == types.AddressingModeLID && !lid.IsEmpty():
			r.Sender = lid
		case mode != types.AddressingModeLID && !pn.IsEmpty() && !lid.IsEmpty():
			r.Sender = pn
		default:
			r.Sender = types.EmptyJID
			continue
		}
		if secret, _, err := client.Store.MsgSecrets.GetMessageSecret(ctx, group, r.Sender, r.ID); err == nil && len(secret) > 0 {
			r.Secret = secret
		}
	}
}
