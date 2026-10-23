//go:build grouphistory

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

var errGroupHistoryNotAllowed = errors.New("only a group admin can share history")

// shareGroupHistoryNative shares recent group messages with members who were
// just added directly. It sends the bundle group-direct and, once WhatsApp has
// acknowledged it, the notice to the group. Returns how many messages were
// shared. NOT reachable from any endpoint yet.
func shareGroupHistoryNative(ctx context.Context, client *whatsmeow.Client, store *MessageStore, group types.JID, receivers []types.JID) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("no message store")
	}
	info, err := client.GetGroupInfo(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("group info: %w", err)
	}
	ownLID := client.Store.GetLID()
	ownPN := client.Store.GetJID()
	isAdmin := false
	for _, p := range info.Participants {
		if (p.JID.User == ownLID.User || p.JID.User == ownPN.User || p.PhoneNumber.User == ownPN.User) && (p.IsAdmin || p.IsSuperAdmin) {
			isAdmin = true
			break
		}
	}
	if !isAdmin {
		return 0, errGroupHistoryNotAllowed
	}

	now := time.Now()
	rows, err := store.loadGroupHistoryRows(group.String(), now.Add(-groupHistoryWindow), groupHistoryMaxMessages*3)
	if err != nil {
		return 0, fmt.Errorf("read messages: %w", err)
	}
	resolveGroupHistorySenders(ctx, client, group, info.AddressingMode, rows)
	rows = selectGroupHistoryRows(rows, now, groupHistoryWindow, groupHistoryMaxMessages)
	if len(rows) == 0 {
		return 0, fmt.Errorf("no messages to share")
	}

	body, err := packGroupHistory(buildGroupHistory(group, ownLID, rows))
	if err != nil {
		return 0, err
	}
	up, err := client.Upload(ctx, body, whatsmeow.MediaGroupHistory)
	if err != nil {
		return 0, fmt.Errorf("upload history: %w", err)
	}
	meta := groupHistoryMetadata(receivers, rows)
	if _, err := client.SendMessage(ctx, group, buildGroupHistoryBundle(up, meta), whatsmeow.SendRequestExtra{
		GroupHistoryReceivers: receivers,
	}); err != nil {
		return 0, fmt.Errorf("send history bundle: %w", err)
	}
	if _, err := client.SendMessage(ctx, group, buildGroupHistoryNotice(meta)); err != nil {
		return len(rows), fmt.Errorf("history shared, but the group notice failed: %w", err)
	}
	return len(rows), nil
}
