package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func lidImageEvent(chat types.JID) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   chat,
				Sender: types.NewJID("60132097728", types.DefaultUserServer),
			},
			ID: "A5107371954C84E870F76619393B2ABA",
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("")}},
	}
}

// A 1:1 chat WhatsApp addressed by LID is stored under the phone JID; the
// webhook must name that same chat, or a consumer's later lookup misses.
func TestWebhookPayloadUsesStoredChat(t *testing.T) {
	lid := types.NewJID("70056000123010", types.HiddenUserServer)
	stored := "601110929928@s.whatsapp.net"

	p := buildWebhookPayload(nil, lidImageEvent(lid), stored)

	if got := p["chat_jid"]; got != stored {
		t.Fatalf("chat_jid = %v, want the stored chat %s", got, stored)
	}
	if got := p["chat_lid"]; got != lid.String() {
		t.Fatalf("chat_lid = %v, want the raw LID %s", got, lid.String())
	}
	if got := p["message_type"]; got != "image" {
		t.Fatalf("message_type = %v, want image (rest of the payload unchanged)", got)
	}
}

// When the store kept the chat as WhatsApp sent it (a phone chat, a group, or a
// LID whose phone is not known yet), there is nothing to add.
func TestWebhookPayloadNoChatLidWhenUnchanged(t *testing.T) {
	for _, chat := range []types.JID{
		types.NewJID("601110929928", types.DefaultUserServer),
		types.NewJID("120363412548814507", types.GroupServer),
		types.NewJID("70056000123010", types.HiddenUserServer),
	} {
		p := buildWebhookPayload(nil, lidImageEvent(chat), chat.String())
		if got := p["chat_jid"]; got != chat.String() {
			t.Fatalf("%s: chat_jid = %v, want it unchanged", chat, got)
		}
		if _, ok := p["chat_lid"]; ok {
			t.Fatalf("%s: chat_lid set although the chat was not rewritten", chat)
		}
	}
}
