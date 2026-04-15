package server

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Feed manages the block data for all channels.
type Feed struct {
	mu               sync.RWMutex
	marker           [protocol.MarkerSize]byte
	channels         []string
	blocks           map[int][][]byte
	lastIDs          map[int]uint32
	contentHashes    map[int]uint32
	chatTypes        map[int]protocol.ChatType
	canSend          map[int]bool
	thumbMIMEs       map[int]string
	thumbB64         map[int]string
	metaBlocks       [][]byte // metadata for all channels
	versionBlocks    [][]byte // channel for latest server-known release version
	updated          time.Time
	telegramLoggedIn bool
	nextFetch        uint32
	latestVersion    string
}

// NewFeed creates a new Feed with the given channel names.
func NewFeed(channels []string) *Feed {
	f := &Feed{
		channels:      channels,
		blocks:        make(map[int][][]byte),
		lastIDs:       make(map[int]uint32),
		contentHashes: make(map[int]uint32),
		chatTypes:     make(map[int]protocol.ChatType),
		canSend:       make(map[int]bool),
		thumbMIMEs:    make(map[int]string),
		thumbB64:      make(map[int]string),
	}
	f.rotateMarker()
	f.rebuildMetaBlocks()
	f.rebuildVersionBlocks()
	return f
}

func (f *Feed) rotateMarker() {
	rand.Read(f.marker[:])
}

// UpdateChannel replaces the messages for a channel, re-serializing into blocks.
func (f *Feed) UpdateChannel(channelNum int, msgs []protocol.Message) {
	data := protocol.SerializeMessages(msgs)
	compressed := protocol.CompressMessages(data)
	blocks := protocol.SplitIntoBlocks(compressed)

	var lastID uint32
	if len(msgs) > 0 {
		lastID = msgs[0].ID
	}
	contentHash := protocol.ContentHashOf(msgs)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.blocks[channelNum] = blocks
	f.lastIDs[channelNum] = lastID
	f.contentHashes[channelNum] = contentHash
	f.updated = time.Now()
	f.rotateMarker()
	f.rebuildMetaBlocks()
}

// GetBlock returns the block data for a given channel and block number.
func (f *Feed) GetBlock(channel, block int) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if channel == protocol.MetadataChannel {
		return f.getMetadataBlock(block)
	}
	if channel == int(protocol.VersionChannel) {
		return f.getVersionBlock(block)
	}

	ch, ok := f.blocks[channel]
	if !ok {
		return nil, fmt.Errorf("channel %d not found", channel)
	}
	if block < 0 || block >= len(ch) {
		return nil, fmt.Errorf("block %d out of range (channel %d has %d blocks)", block, channel, len(ch))
	}
	return ch[block], nil
}

func (f *Feed) getVersionBlock(block int) ([]byte, error) {
	blocks := f.versionBlocks
	if len(blocks) == 0 {
		f.rebuildVersionBlocks()
		blocks = f.versionBlocks
	}
	if block < 0 || block >= len(blocks) {
		return nil, fmt.Errorf("version block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

func (f *Feed) getMetadataBlock(block int) ([]byte, error) {
	blocks := f.metaBlocks
	if len(blocks) == 0 {
		f.rebuildMetaBlocks()
		blocks = f.metaBlocks
	}
	if block < 0 || block >= len(blocks) {
		return nil, fmt.Errorf("metadata block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

// rebuildMetaBlocks re-serializes the metadata and splits it into blocks.
// Must be called with f.mu held.
func (f *Feed) rebuildMetaBlocks() {
	meta := protocol.Metadata{
		Marker:           f.marker,
		Timestamp:        uint32(time.Now().Unix()),
		NextFetch:        f.nextFetch,
		TelegramLoggedIn: f.telegramLoggedIn,
	}

	for i, name := range f.channels {
		chNum := i + 1
		blocks, ok := f.blocks[chNum]
		blockCount := uint16(0)
		if ok {
			blockCount = uint16(len(blocks))
		}
		meta.Channels = append(meta.Channels, protocol.ChannelInfo{
			Name:        name,
			Blocks:      blockCount,
			LastMsgID:   f.lastIDs[chNum],
			ContentHash: f.contentHashes[chNum],
			ChatType:    f.chatTypes[chNum],
			CanSend:     f.canSend[chNum],
			ThumbMIME:   f.thumbMIMEs[chNum],
			ThumbB64:    f.thumbB64[chNum],
		})
	}

	f.metaBlocks = protocol.SplitIntoBlocks(protocol.SerializeMetadata(&meta))
}

func (f *Feed) rebuildVersionBlocks() {
	block, err := protocol.EncodeVersionData(f.latestVersion)
	if err != nil {
		block = make([]byte, protocol.MinBlockPayload)
	}
	f.versionBlocks = [][]byte{block}
}

// SetLatestVersion stores latest known release version for the dedicated version channel.
func (f *Feed) SetLatestVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestVersion = v
	f.rebuildVersionBlocks()
}

// ChannelNames returns the configured channel names.
func (f *Feed) ChannelNames() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]string, len(f.channels))
	copy(result, f.channels)
	return result
}

// SetTelegramLoggedIn sets the flag indicating whether the server has a Telegram session.
func (f *Feed) SetTelegramLoggedIn(loggedIn bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.telegramLoggedIn = loggedIn
	f.rebuildMetaBlocks()
}

// SetNextFetch sets the unix timestamp of the next server-side fetch.
func (f *Feed) SetNextFetch(ts uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFetch = ts
	f.rebuildMetaBlocks()
}

// SetChatInfo stores the chat type and send capability for a channel.
func (f *Feed) SetChatInfo(channelNum int, chatType protocol.ChatType, canSend bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatTypes[channelNum] = chatType
	f.canSend[channelNum] = canSend
	f.rebuildMetaBlocks()
}

// SetChannelThumbnail stores the inline thumbnail payload for metadata delivery.
func (f *Feed) SetChannelThumbnail(channelNum int, mimeType, base64Data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thumbMIMEs[channelNum] = strings.TrimSpace(mimeType)
	f.thumbB64[channelNum] = strings.TrimSpace(base64Data)
	f.rebuildMetaBlocks()
}

// IsPrivateChannel returns true if the channel has chatType == ChatTypePrivate.
func (f *Feed) IsPrivateChannel(channelNum int) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.chatTypes[channelNum] == protocol.ChatTypePrivate
}

// SetChannels replaces the channel list and rebuilds metadata.
func (f *Feed) SetChannels(channels []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = channels
	f.rebuildMetaBlocks()
}
