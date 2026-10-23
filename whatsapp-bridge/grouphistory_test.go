package main

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waGroupHistory"
	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/hkdf"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	ghGroup  = types.NewJID("120363426166419578", types.GroupServer)
	ghOwnLID = types.JID{User: "226993333350451", Server: types.HiddenUserServer, Device: 46}
	ghAlice  = types.NewJID("136365312323759", types.HiddenUserServer)
	ghNewbie = types.JID{User: "11901991923926", Server: types.HiddenUserServer, Device: 3}
	ghNow    = time.Unix(1792723187, 0)
)

func TestSelectGroupHistoryRows(t *testing.T) {
	at := func(d time.Duration) time.Time { return ghNow.Add(-d) }
	rows := []groupHistoryRow{
		{ID: "old", Content: "x", Time: at(15 * 24 * time.Hour), IsFromMe: true},
		{ID: "media", Content: "caption", MediaType: "image", Time: at(time.Hour), IsFromMe: true},
		{ID: "deleted", Content: "x", Time: at(time.Hour), IsFromMe: true, Deleted: true},
		{ID: "failed", Content: "x", Time: at(time.Hour), IsFromMe: true, Failed: true},
		{ID: "future", Content: "x", Time: ghNow.Add(time.Hour), IsFromMe: true},
		{ID: "unresolved", Content: "x", Time: at(time.Hour)},
		{ID: "empty", Time: at(time.Hour), IsFromMe: true},
		{ID: "a", Content: "first", Time: at(3 * time.Hour), Sender: ghAlice},
		{ID: "b", Content: "second", Time: at(2 * time.Hour), IsFromMe: true},
		{ID: "b", Content: "dup", Time: at(2 * time.Hour), IsFromMe: true},
	}
	got := selectGroupHistoryRows(rows, ghNow, groupHistoryWindow, groupHistoryMaxMessages)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("want [b a] newest first, got %+v", got)
	}

	many := make([]groupHistoryRow, 150)
	for i := range many {
		many[i] = groupHistoryRow{ID: fmt.Sprint(i), Content: "m", Time: at(time.Duration(i) * time.Minute), IsFromMe: true}
	}
	got = selectGroupHistoryRows(many, ghNow, groupHistoryWindow, groupHistoryMaxMessages)
	if len(got) != groupHistoryMaxMessages || got[0].ID != "0" || got[99].ID != "99" {
		t.Fatalf("want the newest 100, got %d starting %s", len(got), got[0].ID)
	}
}

// The expectations below are the field-by-field shape of the GroupHistory a
// phone sent on 2026-10-23 (see docs/group-history-sender-plan.md in
// wa-manager-go).
func TestBuildGroupHistoryMatchesPhoneShape(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	rows := []groupHistoryRow{
		{ID: "3EB0OWN", Content: "mine", Time: ghNow.Add(-time.Minute), IsFromMe: true},
		{ID: "ACOTHER", Content: "theirs", Time: ghNow.Add(-2 * time.Minute), Sender: ghAlice, Secret: secret},
		{ID: "ACNOSECRET", Content: "theirs too", Time: ghNow.Add(-3 * time.Minute), Sender: ghAlice},
	}
	gh := buildGroupHistory(ghGroup, ghOwnLID, rows)
	if n := len(gh.GetMessages()); n != 3 {
		t.Fatalf("want 3 messages, got %d", n)
	}
	for i, m := range gh.GetMessages() {
		if m.GetKey().GetRemoteJID() != ghGroup.String() || m.GetKey().GetID() != rows[i].ID {
			t.Errorf("#%d key = %v", i, m.GetKey())
		}
		if m.Status != nil {
			t.Errorf("#%d status should be unset, got %v", i, m.GetStatus())
		}
		if m.GetInteractiveMessageAdditionalMetadata() == nil || m.GetInteractiveMessageAdditionalMetadata().IsGalaxyFlowCompleted == nil {
			t.Errorf("#%d missing interactiveMessageAdditionalMetadata", i)
		}
		if m.GetMessageTimestamp() != uint64(rows[i].Time.Unix()) {
			t.Errorf("#%d timestamp = %d", i, m.GetMessageTimestamp())
		}
		if m.GetMessage().GetConversation() != rows[i].Content {
			t.Errorf("#%d text = %q", i, m.GetMessage().GetConversation())
		}
	}

	own := gh.Messages[0]
	if !own.GetKey().GetFromMe() || own.GetKey().Participant != nil || own.Participant != nil {
		t.Errorf("own message: want fromMe and no participant, got %v / %v", own.GetKey(), own.Participant)
	}
	if own.GetOriginalSelfAuthorUserJIDString() != "226993333350451@lid" {
		t.Errorf("own message: originalSelfAuthor = %q", own.GetOriginalSelfAuthorUserJIDString())
	}
	if own.MessageSecret != nil || own.GetMessage().GetMessageContextInfo() != nil {
		t.Errorf("own message must carry no secret")
	}

	other := gh.Messages[1]
	if other.GetKey().GetFromMe() || other.GetKey().GetParticipant() != ghAlice.String() || other.GetParticipant() != ghAlice.String() {
		t.Errorf("other message: participant = %q / %q", other.GetKey().GetParticipant(), other.GetParticipant())
	}
	if other.OriginalSelfAuthorUserJIDString != nil {
		t.Errorf("other message must not carry originalSelfAuthor")
	}
	if !bytes.Equal(other.GetMessageSecret(), secret) || !bytes.Equal(other.GetMessage().GetMessageContextInfo().GetMessageSecret(), secret) {
		t.Errorf("other message: secret must be on both the WMI and messageContextInfo")
	}
	if noSecret := gh.Messages[2]; noSecret.MessageSecret != nil || noSecret.GetMessage().GetMessageContextInfo() != nil {
		t.Errorf("unknown secret must be left out, not sent empty")
	}
}

func TestGroupHistoryMetadata(t *testing.T) {
	rows := []groupHistoryRow{
		{ID: "n", Time: ghNow.Add(-time.Minute)},
		{ID: "o", Time: ghNow.Add(-time.Hour)},
	}
	meta := groupHistoryMetadata([]types.JID{ghNewbie}, rows)
	if got := meta.GetHistoryReceivers(); len(got) != 1 || got[0] != "11901991923926@lid" {
		t.Fatalf("receivers = %v", got)
	}
	if meta.GetMessageCount() != 2 || meta.GetOldestMessageTimestampInWindow() != ghNow.Add(-time.Hour).Unix() {
		t.Fatalf("meta = %v", meta)
	}
	if meta.OldestMessageTimestampInBundle != nil || meta.NonHistoryReceivers != nil {
		t.Fatalf("fields the phone did not send must stay unset: %v", meta)
	}
}

// TestGroupHistoryRoundTrip packs, encrypts the way an upload does (HKDF info
// "Group History"), then decrypts and inflates it back. With
// GH_ROUNDTRIP_DIR set it also writes a MSG-DEBUG style line and the .enc file
// so the scratchpad ghdecode tool can decode it like a captured bundle.
func TestGroupHistoryRoundTrip(t *testing.T) {
	rows := []groupHistoryRow{
		{ID: "3EB0OWN", Content: "mine", Time: ghNow.Add(-time.Minute), IsFromMe: true},
		{ID: "ACOTHER", Content: "theirs", Time: ghNow.Add(-2 * time.Minute), Sender: ghAlice, Secret: bytes.Repeat([]byte{7}, 32)},
	}
	gh := buildGroupHistory(ghGroup, ghOwnLID, rows)
	body, err := packGroupHistory(gh)
	if err != nil {
		t.Fatal(err)
	}

	up, enc := encryptForTest(t, body, whatsmeow.MediaType("Group History"))
	bundle := buildGroupHistoryBundle(up, groupHistoryMetadata([]types.JID{ghNewbie}, rows))
	b := bundle.GetMessageHistoryBundle()
	if b.GetMimetype() != "application/protobuf" || len(bundle.GetMessageContextInfo().GetMessageSecret()) != 32 {
		t.Fatalf("bundle envelope = %v", bundle)
	}
	if d := b.GetContextInfo().GetMediaDomainInfo(); d.GetMediaKeyDomain() != waE2E.MediaKeyDomain_MEDIA_KEY_DOMAIN_E2EE || !bytes.Equal(d.GetE2EeMediaKey(), b.GetMediaKey()) {
		t.Fatalf("bundle must mark its key as an E2EE media key like the phone does, got %v", d)
	}

	plain := decryptForTest(t, enc, b.GetMediaKey())
	zr, err := zlib.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	inflated, _ := io.ReadAll(zr)
	var back waGroupHistory.GroupHistory
	if err := proto.Unmarshal(inflated, &back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&back, gh) {
		t.Fatalf("round trip changed the body")
	}

	notice := buildGroupHistoryNotice(b.GetMessageHistoryMetadata())
	if !proto.Equal(notice.GetMessageHistoryNotice().GetMessageHistoryMetadata(), b.GetMessageHistoryMetadata()) {
		t.Fatalf("notice and bundle metadata differ")
	}

	if dir := os.Getenv("GH_ROUNDTRIP_DIR"); dir != "" {
		raw, _ := protojson.Marshal(bundle)
		line := "[bridge Test:0] 00:00:00.000 [Client INFO] MSG-DEBUG id=TEST chat=" + ghGroup.String() + " type=media cat= raw=" + string(raw) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "built_bundle.txt"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "built.enc"), enc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func groupHistoryKeys(t *testing.T, mediaKey []byte, info whatsmeow.MediaType) (iv, cipherKey, macKey []byte) {
	t.Helper()
	keys := make([]byte, 112)
	if _, err := io.ReadFull(hkdf.New(sha256.New, mediaKey, nil, []byte(info)), keys); err != nil {
		t.Fatal(err)
	}
	return keys[:16], keys[16:48], keys[48:80]
}

// encryptForTest mirrors whatsmeow's Upload encryption without the network.
func encryptForTest(t *testing.T, plain []byte, info whatsmeow.MediaType) (whatsmeow.UploadResponse, []byte) {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	iv, cipherKey, macKey := groupHistoryKeys(t, key, info)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(cipherKey)
	data := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, padded)
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(data)
	enc := append(data, h.Sum(nil)[:10]...)
	plainSum, encSum := sha256.Sum256(plain), sha256.Sum256(enc)
	return whatsmeow.UploadResponse{
		DirectPath:    "/v/t62.test/group-history.enc",
		MediaKey:      key,
		FileSHA256:    plainSum[:],
		FileEncSHA256: encSum[:],
		FileLength:    uint64(len(plain)),
	}, enc
}

func decryptForTest(t *testing.T, enc, mediaKey []byte) []byte {
	t.Helper()
	iv, cipherKey, macKey := groupHistoryKeys(t, mediaKey, whatsmeow.MediaType("Group History"))
	data, tag := enc[:len(enc)-10], enc[len(enc)-10:]
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(data)
	if !hmac.Equal(h.Sum(nil)[:10], tag) {
		t.Fatal("MAC mismatch")
	}
	block, _ := aes.NewCipher(cipherKey)
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return out[:len(out)-int(out[len(out)-1])]
}
