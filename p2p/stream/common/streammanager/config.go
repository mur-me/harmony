package streammanager

import (
	"time"

	libp2p_peer "github.com/libp2p/go-libp2p/core/peer"
)

const (
	// checkInterval is the default interval for checking stream number. If the stream
	// number is smaller than softLoCap, an active discover through DHT will be triggered.
	checkInterval = 30 * time.Second
	// discTimeout is the timeout for one batch of discovery
	discTimeout = 10 * time.Second
	// connectTimeout is the timeout for setting up a stream with a discovered peer
	connectTimeout = 60 * time.Second
	// MaxReservedStreams is the maximum number of reserved streams
	MaxReservedStreams = 100
	// RemovalCooldownDuration defines the cooldown period (in minutes) before a removed stream can reconnect.
	RemovalCooldownDuration    = 5 * time.Minute
	MaxRemovalCooldownDuration = 60 * time.Minute

	// setupConcurrency limits concurrent stream setup goroutines
	setupConcurrency = 16
	// trustedPeersCheckInterval is the interval to check for trusted peers initialization status
	trustedPeersCheckInterval = 500 * time.Millisecond
)

// Config is the config for stream manager
type Config struct {
	// HardLoCap is low cap of stream number that immediately trigger discovery
	HardLoCap int
	// SoftLoCap is low cap of stream number that will trigger discovery during stream check
	SoftLoCap int
	// HiCap is the high cap of stream number
	HiCap int
	// DiscBatch is the size of each discovery
	DiscBatch int
	// TrustedPeers is a function that returns peer IDs considered trusted.
	// This allows dynamic updates when trusted peers are added after initialization.
	// If nil, no trusted peers will be used.
	TrustedPeers func() map[libp2p_peer.ID]struct{}
	// TrustedPeersInitiated is a function that returns true if trusted peers initialization is complete.
	// The stream manager waits for this to return true before starting bootstrap discovery.
	// If nil, the stream manager will not wait for trusted peers.
	TrustedPeersInitiated func() bool
}
