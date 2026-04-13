package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// PublicReader fetches recent posts from public Telegram channels via the web view.
type PublicReader struct {
	channels []string
	feed     *Feed
	msgLimit int
	// imageMaxKB controls the maximum downloaded image size per post when
	// embedding image data directly into DNS-delivered message text.
	imageMaxKB int
	baseCh     int

	client  *http.Client
	baseURL string

	mu       sync.RWMutex
	cache    map[string]cachedMessages
	cacheTTL time.Duration

	refreshCh chan struct{}
}

// NewPublicReader creates a reader for public channels without Telegram login.
func NewPublicReader(channelUsernames []string, feed *Feed, msgLimit int, imageMaxKB int, baseCh int) *PublicReader {
	cleaned := make([]string, len(channelUsernames))
	for i, u := range channelUsernames {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	if msgLimit <= 0 {
		msgLimit = 15
	}
	if baseCh <= 0 {
		baseCh = 1
	}
	if imageMaxKB < 0 {
		imageMaxKB = 500
	}
	return &PublicReader{
		channels:   cleaned,
		feed:       feed,
		msgLimit:   msgLimit,
		imageMaxKB: imageMaxKB,
		baseCh:     baseCh,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL:   "https://t.me/s",
		cache:     make(map[string]cachedMessages),
		cacheTTL:  10 * time.Minute,
		refreshCh: make(chan struct{}, 1),
	}
}

// Run starts the periodic public-channel fetch loop.
func (pr *PublicReader) Run(ctx context.Context) error {
	pr.feed.SetTelegramLoggedIn(false)
	pr.fetchAll(ctx)

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	pr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pr.fetchAll(ctx)
			pr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))
		case <-pr.refreshCh:
			pr.mu.Lock()
			pr.cache = make(map[string]cachedMessages)
			pr.mu.Unlock()
			pr.fetchAll(ctx)
			ticker.Reset(10 * time.Minute)
			pr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))
		}
	}
}

// RequestRefresh signals the fetch loop to re-fetch immediately.
func (pr *PublicReader) RequestRefresh() {
	select {
	case pr.refreshCh <- struct{}{}:
	default:
	}
}

// UpdateChannels replaces the channel list and updates Feed metadata.
func (pr *PublicReader) UpdateChannels(channels []string) {
	cleaned := make([]string, len(channels))
	for i, u := range channels {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	pr.mu.Lock()
	pr.channels = cleaned
	pr.cache = make(map[string]cachedMessages)
	pr.mu.Unlock()
}

func (pr *PublicReader) fetchAll(ctx context.Context) {
	for i, username := range pr.channels {
		chNum := pr.baseCh + i

		pr.mu.RLock()
		cached, ok := pr.cache[username]
		pr.mu.RUnlock()
		if ok && time.Since(cached.fetched) < pr.cacheTTL {
			continue
		}

		msgs, err := pr.fetchChannel(ctx, username)
		if err != nil {
			log.Printf("[public] fetch %s: %v", username, err)
			continue
		}

		// Merge new messages with previously cached ones to accumulate history.
		if ok && len(cached.msgs) > 0 {
			msgs = mergeMessages(cached.msgs, msgs)
		}
		if pr.msgLimit > 0 && len(msgs) > pr.msgLimit {
			msgs = msgs[:pr.msgLimit]
		}

		pr.mu.Lock()
		pr.cache[username] = cachedMessages{msgs: msgs, fetched: time.Now()}
		pr.mu.Unlock()

		pr.feed.UpdateChannel(chNum, msgs)
		pr.feed.SetChatInfo(chNum, protocol.ChatTypeChannel, false)
		log.Printf("[public] updated %s: %d messages", username, len(msgs))
	}
}

func (pr *PublicReader) fetchChannel(ctx context.Context, username string) ([]protocol.Message, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.baseURL+"/"+url.PathEscape(username), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")

	resp, err := pr.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	msgs, err := parsePublicMessages(body)
	if err != nil {
		return nil, err
	}
	return pr.embedImagesInMessages(ctx, msgs), nil
}

type publicMessage struct {
	id        uint32
	timestamp uint32
	text      string
}

// mergeMessages combines old cached messages with newly fetched ones.
// New messages win on ID conflicts (edits). Result is sorted by ID descending.
func mergeMessages(old, new []protocol.Message) []protocol.Message {
	byID := make(map[uint32]protocol.Message, len(old)+len(new))
	for _, m := range old {
		byID[m.ID] = m
	}
	for _, m := range new {
		byID[m.ID] = m // new overwrites old (edits)
	}
	merged := make([]protocol.Message, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].ID > merged[j].ID
	})
	return merged
}

func (pr *PublicReader) embedImagesInMessages(ctx context.Context, msgs []protocol.Message) []protocol.Message {
	if len(msgs) == 0 || pr.imageMaxKB == 0 {
		return msgs
	}
	out := make([]protocol.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		urlVal := extractImageURLMarker(out[i].Text)
		if urlVal == "" {
			continue
		}
		mimeType, encoded, byteLen := pr.fetchInlineImage(ctx, urlVal)
		if encoded == "" {
			continue
		}
		out[i].Text = replaceImageURLWithInlineData(out[i].Text, mimeType, encoded, byteLen)
	}
	return out
}

func (pr *PublicReader) fetchInlineImage(ctx context.Context, rawURL string) (mimeType, encoded string, byteLen int) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", 0
	}
	maxBytes := int64(pr.imageMaxKB) * 1024
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", 0
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")

	resp, err := pr.client.Do(req)
	if err != nil {
		return "", "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !strings.HasPrefix(ct, "image/") {
		return "", "", 0
	}
	if resp.ContentLength > maxBytes {
		return "", "", 0
	}
	var buf bytes.Buffer
	limited := io.LimitReader(resp.Body, maxBytes+1)
	if _, err := io.Copy(&buf, limited); err != nil {
		return "", "", 0
	}
	if int64(buf.Len()) > maxBytes {
		return "", "", 0
	}
	if buf.Len() == 0 {
		return "", "", 0
	}
	return ct, base64.StdEncoding.EncodeToString(buf.Bytes()), buf.Len()
}

func extractImageURLMarker(text string) string {
	idx := strings.Index(text, "\n[IMG_URL]")
	if idx == -1 {
		return ""
	}
	return strings.TrimSpace(text[idx+len("\n[IMG_URL]"):])
}

func replaceImageURLWithInlineData(text, mimeType, encoded string, byteLen int) string {
	idx := strings.Index(text, "\n[IMG_URL]")
	if idx == -1 {
		return text
	}
	prefix := text[:idx]
	return fmt.Sprintf("%s\n[IMG_MIME]%s\n[IMG_SIZE]%d\n[IMG_B64]%s", prefix, mimeType, byteLen, encoded)
}

func parsePublicMessages(body []byte) ([]protocol.Message, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	var collected []publicMessage
	visitNodes(doc, func(n *html.Node) {
		post := attrValue(n, "data-post")
		if post == "" {
			return
		}
		id, err := parsePostID(post)
		if err != nil {
			return
		}
		text := strings.TrimSpace(extractMessageText(findMessageBodyNode(n)))
		mediaPrefix := ""
		imageURL := ""
		switch {
		case findFirstByClass(n, "tgme_widget_message_photo_wrap") != nil:
			mediaPrefix = protocol.MediaImage
			imageURL = extractPhotoURL(n)
		case findFirstByClass(n, "tgme_widget_message_video_player") != nil ||
			findFirstByClass(n, "tgme_widget_message_roundvideo_player") != nil:
			mediaPrefix = protocol.MediaVideo
		case findFirstByClass(n, "tgme_widget_message_sticker_wrap") != nil:
			mediaPrefix = protocol.MediaSticker
		case findFirstByClass(n, "tgme_widget_message_voice") != nil:
			mediaPrefix = protocol.MediaAudio
		case findFirstByClass(n, "tgme_widget_message_poll") != nil:
			mediaPrefix = protocol.MediaPoll
		case findFirstByClass(n, "tgme_widget_message_location_wrap") != nil ||
			findFirstByClass(n, "tgme_widget_message_venue_wrap") != nil:
			mediaPrefix = protocol.MediaLocation
		case findFirstByClass(n, "tgme_widget_message_contact_wrap") != nil:
			mediaPrefix = protocol.MediaContact
		case findFirstByClass(n, "tgme_widget_message_document_wrap") != nil:
			mediaPrefix = protocol.MediaFile
		}
		if mediaPrefix != "" {
			if text != "" {
				text = mediaPrefix + "\n" + text
			} else {
				text = mediaPrefix
			}
			if imageURL != "" {
				text += "\n[IMG_URL]" + imageURL
			}
		}
		if text == "" {
			return
		}
		collected = append(collected, publicMessage{
			id:        id,
			timestamp: extractMessageTimestamp(n),
			text:      text,
		})
	})

	if len(collected) == 0 {
		return nil, fmt.Errorf("no public messages found")
	}

	sort.Slice(collected, func(i, j int) bool {
		return collected[i].id > collected[j].id
	})

	msgs := make([]protocol.Message, 0, len(collected))
	for _, msg := range collected {
		msgs = append(msgs, protocol.Message{ID: msg.id, Timestamp: msg.timestamp, Text: msg.text})
	}
	return msgs, nil
}

func extractPhotoURL(n *html.Node) string {
	photo := findFirstByClass(n, "tgme_widget_message_photo_wrap")
	if photo == nil {
		return ""
	}
	if style := attrValue(photo, "style"); style != "" {
		if u := extractURLFromInlineStyle(style); u != "" {
			return u
		}
	}
	if href := strings.TrimSpace(attrValue(photo, "href")); strings.HasPrefix(href, "https://") {
		return href
	}
	return ""
}

func extractURLFromInlineStyle(style string) string {
	style = strings.TrimSpace(style)
	start := strings.Index(style, "url(")
	if start == -1 {
		return ""
	}
	rest := style[start+4:]
	end := strings.Index(rest, ")")
	if end == -1 {
		return ""
	}
	raw := strings.TrimSpace(rest[:end])
	raw = strings.Trim(raw, `"'`)
	if strings.HasPrefix(raw, "https://") {
		return raw
	}
	return ""
}

func visitNodes(n *html.Node, fn func(*html.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		visitNodes(child, fn)
	}
}

func findFirstByClass(n *html.Node, class string) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found != nil {
			return
		}
		if hasClass(cur, class) {
			found = cur
		}
	})
	return found
}

func hasClass(n *html.Node, class string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, attr := range n.Attr {
		if attr.Key != "class" {
			continue
		}
		for _, token := range strings.Fields(attr.Val) {
			if token == class {
				return true
			}
		}
	}
	return false
}

func attrValue(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func parsePostID(post string) (uint32, error) {
	idx := strings.LastIndex(post, "/")
	if idx == -1 || idx+1 >= len(post) {
		return 0, fmt.Errorf("invalid post id")
	}
	id, err := strconv.ParseUint(post[idx+1:], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(id), nil
}

func extractMessageTimestamp(n *html.Node) uint32 {
	timeNode := findFirstByClass(n, "tgme_widget_message_date")
	if timeNode == nil {
		timeNode = findFirstElement(n, "time")
	}
	if timeNode == nil {
		return uint32(time.Now().Unix())
	}
	datetime := attrValue(timeNode, "datetime")
	if datetime == "" {
		timeChild := findFirstElement(timeNode, "time")
		datetime = attrValue(timeChild, "datetime")
	}
	if datetime == "" {
		return uint32(time.Now().Unix())
	}
	ts, err := time.Parse(time.RFC3339, datetime)
	if err != nil {
		return uint32(time.Now().Unix())
	}
	return uint32(ts.Unix())
}

func findFirstElement(n *html.Node, tag string) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found == nil && cur.Type == html.ElementNode && cur.Data == tag {
			found = cur
		}
	})
	return found
}

// findMessageBodyNode returns the main post text node while skipping reply preview
// snippets. In Telegram public HTML, quoted/replied text may appear before the
// real message body and can otherwise be mistakenly parsed as the post text.
func findMessageBodyNode(n *html.Node) *html.Node {
	var found *html.Node
	visitNodes(n, func(cur *html.Node) {
		if found != nil || !hasClass(cur, "tgme_widget_message_text") {
			return
		}
		for p := cur.Parent; p != nil; p = p.Parent {
			if hasClass(p, "tgme_widget_message_reply") {
				return
			}
		}
		found = cur
	})
	if found != nil {
		return found
	}
	return findFirstByClass(n, "tgme_widget_message_text")
}

func extractMessageText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur == nil {
			return
		}
		if cur.Type == html.TextNode {
			text := strings.TrimSpace(cur.Data)
			if text != "" {
				if b.Len() > 0 {
					last := b.String()[b.Len()-1]
					if last != '\n' && last != ' ' {
						b.WriteByte(' ')
					}
				}
				b.WriteString(text)
			}
		}
		if cur.Type == html.ElementNode && cur.Data == "br" {
			trimTrailingSpace(&b)
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.ReplaceAll(b.String(), " \n", "\n"))
}

func trimTrailingSpace(b *strings.Builder) {
	s := b.String()
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	b.Reset()
	b.WriteString(s)
}
