package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func TestParsePublicMessages(t *testing.T) {
	body := []byte(`
		<html><body>
		<div class="tgme_widget_message" data-post="testchan/101">
			<a class="tgme_widget_message_date"><time datetime="2026-03-30T04:45:00+00:00"></time></a>
			<div class="tgme_widget_message_text">hello<br/>world</div>
		</div>
		<div class="tgme_widget_message" data-post="testchan/105">
			<a class="tgme_widget_message_date"><time datetime="2026-03-30T04:50:00+00:00"></time></a>
			<div class="tgme_widget_message_photo_wrap"></div>
		</div>
		<div class="tgme_widget_message" data-post="testchan/106">
			<a class="tgme_widget_message_date"><time datetime="2026-03-30T04:51:00+00:00"></time></a>
			<a class="tgme_widget_message_photo_wrap" href="https://t.me/testchan/106"></a>
			<div class="tgme_widget_message_text">photo caption</div>
		</div>
		</body></html>
	`)

	msgs, err := parsePublicMessages(body)
	if err != nil {
		t.Fatalf("parsePublicMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}
	// Photo with caption (newest first)
	if msgs[0].ID != 106 {
		t.Fatalf("msgs[0].ID = %d, want 106", msgs[0].ID)
	}
	want := protocol.MediaImage + "\n" + "photo caption\n[IMG_URL]https://t.me/testchan/106"
	if msgs[0].Text != want {
		t.Fatalf("msgs[0].Text = %q, want %q", msgs[0].Text, want)
	}
	// Photo only
	if msgs[1].ID != 105 {
		t.Fatalf("msgs[1].ID = %d, want 105", msgs[1].ID)
	}
	if msgs[1].Text != protocol.MediaImage {
		t.Fatalf("msgs[1].Text = %q, want %q", msgs[1].Text, protocol.MediaImage)
	}
	// Text only
	if msgs[2].ID != 101 {
		t.Fatalf("msgs[2].ID = %d, want 101", msgs[2].ID)
	}
	if msgs[2].Text != "hello\nworld" {
		t.Fatalf("msgs[2].Text = %q, want %q", msgs[2].Text, "hello\nworld")
	}
}

func TestExtractURLFromInlineStyle(t *testing.T) {
	got := extractURLFromInlineStyle(`background-image:url('https://cdn4.cdn-telegram.org/file.jpg')`)
	want := "https://cdn4.cdn-telegram.org/file.jpg"
	if got != want {
		t.Fatalf("extractURLFromInlineStyle() = %q, want %q", got, want)
	}
}

func TestParsePublicMessagesNoLimit(t *testing.T) {
	body := []byte(`
		<html><body>
		<div class="tgme_widget_message" data-post="testchan/1"><div class="tgme_widget_message_text">one</div></div>
		<div class="tgme_widget_message" data-post="testchan/2"><div class="tgme_widget_message_text">two</div></div>
		<div class="tgme_widget_message" data-post="testchan/3"><div class="tgme_widget_message_text">three</div></div>
		</body></html>
	`)

	msgs, err := parsePublicMessages(body)
	if err != nil {
		t.Fatalf("parsePublicMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}
	if msgs[0].ID != 3 || msgs[1].ID != 2 || msgs[2].ID != 1 {
		t.Fatalf("got ids %d,%d,%d want 3,2,1", msgs[0].ID, msgs[1].ID, msgs[2].ID)
	}
}

func TestMergeMessages(t *testing.T) {
	old := []protocol.Message{
		{ID: 100, Timestamp: 1000, Text: "old100"},
		{ID: 99, Timestamp: 999, Text: "old99"},
	}
	newMsgs := []protocol.Message{
		{ID: 101, Timestamp: 1001, Text: "new101"},
		{ID: 100, Timestamp: 1000, Text: "edited100"},
	}
	merged := mergeMessages(old, newMsgs)
	if len(merged) != 3 {
		t.Fatalf("len(merged) = %d, want 3", len(merged))
	}
	if merged[0].ID != 101 {
		t.Fatalf("merged[0].ID = %d, want 101", merged[0].ID)
	}
	if merged[1].Text != "edited100" {
		t.Fatalf("merged[1].Text = %q, want edited100", merged[1].Text)
	}
	if merged[2].ID != 99 {
		t.Fatalf("merged[2].ID = %d, want 99", merged[2].ID)
	}
}

func TestParsePublicMessagesReplyPreviewUsesMainBody(t *testing.T) {
	body := []byte(`
		<html><body>
		<div class="tgme_widget_message" data-post="testchan/201">
			<div class="tgme_widget_message_reply">
				<div class="tgme_widget_message_text">old replied message preview</div>
			</div>
			<div class="tgme_widget_message_text">this is the real new post</div>
		</div>
		</body></html>
	`)

	msgs, err := parsePublicMessages(body)
	if err != nil {
		t.Fatalf("parsePublicMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Text != "this is the real new post" {
		t.Fatalf("msgs[0].Text = %q, want %q", msgs[0].Text, "this is the real new post")
	}
}

func TestPublicReaderEmbedImagesInMessages(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(imgBytes)
	}))
	defer srv.Close()
	imageURL := srv.URL

	pr := NewPublicReader(nil, nil, 15, 500, 1)
	pr.client = srv.Client()
	msgs := []protocol.Message{
		{ID: 1, Text: protocol.MediaImage + "\ncaption\n[IMG_URL]" + imageURL},
	}
	out := pr.embedImagesInMessages(context.Background(), "test-channel", msgs)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if strings.Contains(out[0].Text, "[IMG_URL]") {
		t.Fatalf("expected IMG_URL marker to be replaced, got %q", out[0].Text)
	}
	if !strings.Contains(out[0].Text, "\n[IMG_MIME]image/png\n[IMG_SIZE]4\n[IMG_B64]") {
		t.Fatalf("missing inline image markers, got %q", out[0].Text)
	}
}
