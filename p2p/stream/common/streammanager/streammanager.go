package streammanager

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/harmony-one/abool"
	"github.com/harmony-one/harmony/internal/utils"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/libp2p/go-libp2p/core/network"
	libp2p_peer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

var (
	// ErrStreamAlreadyRemoved is the error that a stream has already been removed
	ErrStreamAlreadyRemoved = errors.New("stream already removed")
	// ErrStreamAlreadyExist is the error that a stream has already exist
	ErrStreamAlreadyExist = errors.New("stream already exist")
	// ErrTooManyStreams is the error that the number of streams is exceeded the capacity
	ErrTooManyStreams = errors.New("too many streams")
	// ErrStreamRemovalNotExpired is the error that the stream was removed before and can't be added yet
	ErrStreamRemovalNotExpired = errors.New("stream removal not expired yet")
)

// streamManager is the implementation of StreamManager. It manages streams on
// one single protocol. It does the following job:
// 1. add a new stream.
// 2. closes a stream.
// 3. discover and connect new streams when the number of streams is below threshold.
// 4. emit stream events to inform other modules.
// 5. reset all streams on close.
type streamManager struct {
	// streamManager only manages streams on one protocol.
	myProtoID   sttypes.ProtoID
	myProtoSpec sttypes.ProtoSpec
	config      Config
	// streams is the map of peer ID to stream
	// Note that it could happen that remote node does not share exactly the same
	// protocol ID (e.g. different version)
	streams *streamSet
	// tracks removed streams with cooldown
	removedStreams *sttypes.SafeMap[sttypes.StreamID, *RemovalInfo]
	// reserved streams
	reservedStreams *streamSet
	getTrustedPeers func() map[libp2p_peer.ID]struct{}
	// libp2p utilities
	host         host
	pf           peerFinder
	handleStream func(stream network.Stream)
	// incoming task channels
	addStreamCh chan addStreamTask
	rmStreamCh  chan rmStreamTask
	stopCh      chan stopTask
	discCh      chan discTask
	curTask     interface{}
	coolDown    *abool.AtomicBool
	// utils
	coolDownCache    *coolDownCache
	addStreamFeed    event.Feed
	removeStreamFeed event.Feed
	logger           zerolog.Logger
	ctx              context.Context
	cancel           func()

	// limit concurrent setup of streams
	setupSem chan struct{}
	// function to check if trusted peers initialization is complete
	trustedPeersInitiated func() bool
	// trustedPeersProcessed tracks if trusted peers have been processed during bootstrap.
	// This ensures trusted peers are only processed once during the initial bootstrap discovery.
	trustedPeersProcessed *abool.AtomicBool
}

type RemovalInfo struct {
	count     uint64
	removedAt time.Time
	expireAt  time.Time
	mu        sync.RWMutex
}

// MarkAsRemoved resets the removal time and increments the removal count.
func (rm *RemovalInfo) MarkAsRemoved(criticalErr bool) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	now := time.Now()
	if rm.count > 0 && now.Sub(rm.removedAt) > MaxRemovalCooldownDuration {
		rm.count = 0
	}
	if criticalErr {
		rm.expireAt = now.Add(MaxRemovalCooldownDuration)
	} else {
		// First failure (count=0) gets no cooldown to avoid penalizing one-off disconnects.
		cooldownDur := RemovalCooldownDuration * time.Duration(rm.count)
		if cooldownDur > MaxRemovalCooldownDuration {
			cooldownDur = MaxRemovalCooldownDuration
		}
		rm.expireAt = now.Add(cooldownDur)
	}
	rm.removedAt = now
	rm.count++
}

// RemovedAt returns the timestamp when the stream was removed.
func (rm *RemovalInfo) RemovedAt() time.Time {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	return rm.removedAt
}

// HasExpired checks if the cooldown period has passed, allowing the stream to reconnect.
func (rm *RemovalInfo) HasExpired() bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	return time.Now().After(rm.expireAt)
}

// BumpCount increases the removal count.
func (rm *RemovalInfo) BumpCount() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rm.count++
}

// ResetCount resets the removal count.
func (rm *RemovalInfo) ResetCount() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rm.count = 0
}

// NewStreamManager creates a new stream manager for the given proto ID
func NewStreamManager(pid sttypes.ProtoID, host host, pf peerFinder, handleStream func(network.Stream), c Config) StreamManager {
	return newStreamManager(pid, host, pf, handleStream, c)
}

// newStreamManager creates a new stream manager
func newStreamManager(pid sttypes.ProtoID, host host, pf peerFinder, handleStream func(network.Stream), c Config) *streamManager {
	ctx, cancel := context.WithCancel(context.Background())

	logger := utils.Logger().With().Str("module", "stream manager").
		Str("protocol ID", string(pid)).Logger()

	protoSpec, _ := sttypes.ProtoIDToProtoSpec(pid)

	var trustedPeerCount int
	if c.TrustedPeers != nil {
		trustedPeers := c.TrustedPeers()
		trustedPeerCount = len(trustedPeers)
	}
	if trustedPeerCount > 0 {
		logger.Info().
			Int("trustedPeerCount", trustedPeerCount).
			Msg("[StreamManager] initialized with trusted peers")
	} else {
		logger.Info().
			Msg("[StreamManager] initialized with no trusted peers (or nil function)")
	}

	sm := &streamManager{
		myProtoID:             pid,
		myProtoSpec:           protoSpec,
		config:                c,
		streams:               newStreamSet(),
		reservedStreams:       newStreamSet(),
		removedStreams:        sttypes.NewSafeMap[sttypes.StreamID, *RemovalInfo](),
		getTrustedPeers:       c.TrustedPeers,
		host:                  host,
		pf:                    pf,
		handleStream:          handleStream,
		addStreamCh:           make(chan addStreamTask),
		rmStreamCh:            make(chan rmStreamTask),
		stopCh:                make(chan stopTask),
		discCh:                make(chan discTask, 1), // discCh is a buffered channel to avoid overuse of goroutine
		coolDown:              abool.New(),
		coolDownCache:         newCoolDownCache(),
		logger:                logger,
		ctx:                   ctx,
		cancel:                cancel,
		setupSem:              make(chan struct{}, setupConcurrency),
		trustedPeersInitiated: c.TrustedPeersInitiated,
		trustedPeersProcessed: abool.New(),
	}

	// Initialize all stream metrics with this protocol ID

	// Initialize gauge metrics
	numStreamsGaugeVec.With(prometheus.Labels{"topic": string(pid)}).Set(0)
	numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(pid)}).Set(0)
	numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(pid)}).Set(0)

	// Initialize counter metrics
	discoverCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	discoveredPeersCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	addedStreamsCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	removedStreamsCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	streamCriticalErrorCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	trustedPeerStreamsConnectFailuresCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	trustedPeerStreamsSetupAttemptsCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)
	trustedPeerStreamsAddedCounterVec.With(prometheus.Labels{"topic": string(pid)}).Add(0)

	// Note: streamRemovalReasonCounterVec and setupStreamDuration don't need initialization
	// - streamRemovalReasonCounterVec has dynamic labels (reason, critical) that can't be pre-initialized
	// - setupStreamDuration is a histogram that doesn't need initialization

	return sm
}

// Start starts the stream manager
func (sm *streamManager) Start() {
	go sm.loop()
}

// Close close the stream manager
func (sm *streamManager) Close() {
	task := stopTask{done: make(chan struct{})}
	sm.stopCh <- task

	<-task.done
}

func (sm *streamManager) loop() {
	var (
		discTicker = time.NewTicker(checkInterval)
		discCtx    context.Context
		discCancel func()
	)
	defer discTicker.Stop()

	// Wait for trusted peers to be initialized before bootstrap discovery.
	// This ensures trusted peers are available for the first discovery cycle.
	if sm.trustedPeersInitiated != nil {
		sm.waitForTrustedPeersInitialization()
	}

	// Initialize gauge metrics with current values (metrics already initialized to 0 in constructor)
	numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))
	numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))
	numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))

	// bootstrap discovery
	sm.discCh <- discTask{}

	for {
		select {
		case <-discTicker.C:
			// Periodically refresh gauge metrics
			numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))
			numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))
			numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))

			if !sm.softHaveEnoughStreams() {
				sm.discCh <- discTask{}
			}

		case <-sm.discCh:
			if sm.coolDown.IsSet() {
				sm.logger.Info().Msg("skipping discover for cool down")
				continue
			}
			if discCancel != nil {
				discCancel() // cancel last discovery
			}
			discCtx, discCancel = context.WithCancel(sm.ctx)
			go func(ctx context.Context) {
				discovered, err := sm.discoverAndSetupStream(ctx)
				if err != nil {
					sm.logger.Err(err)
				}
				// Update stream metrics after discovery completes
				numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))
				numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))
				numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))
				if discovered == 0 {
					// start discover cool down
					sm.coolDown.Set()
					go func() {
						time.Sleep(coolDownPeriod)
						sm.coolDown.UnSet()
					}()
				}
			}(discCtx)

		case addStream := <-sm.addStreamCh:
			err := sm.handleAddStream(addStream.st)
			addStream.errC <- err

		case rmStream := <-sm.rmStreamCh:
			err := sm.handleRemoveStream(rmStream.id, rmStream.reason, rmStream.criticalErr)
			rmStream.errC <- err

		case stop := <-sm.stopCh:
			sm.coolDown.Set() // to immediately block discoveries
			if discCancel != nil {
				discCancel()
			}
			sm.cancel()
			sm.removeAllStreamOnClose()
			stop.done <- struct{}{}
			return
		}
	}
}

// NewStream handles a new stream from stream handler protocol
func (sm *streamManager) NewStream(stream sttypes.Stream) error {
	if err := sm.sanityCheckStream(stream); err != nil {
		stream.Close("stream sanity check failed", true)
		return errors.Wrap(err, "stream sanity check failed")
	}
	task := addStreamTask{
		st:   stream,
		errC: make(chan error),
	}
	sm.addStreamCh <- task
	return <-task.errC
}

// RemoveStream close and remove a stream from stream manager
func (sm *streamManager) RemoveStream(stID sttypes.StreamID, reason string, criticalErr bool) error {
	task := rmStreamTask{
		id:          stID,
		reason:      reason,
		criticalErr: criticalErr,
		errC:        make(chan error),
	}
	sm.rmStreamCh <- task
	return <-task.errC
}

// GetStreams return the streams.
func (sm *streamManager) GetStreams() []sttypes.Stream {
	return sm.streams.getStreams()
}

// GetStreamByID return the stream with the given id.
func (sm *streamManager) GetStreamByID(id sttypes.StreamID) (sttypes.Stream, bool) {
	return sm.streams.get(id)
}

// GetReservedStreams return the reserved streams.
func (sm *streamManager) GetReservedStreams() []sttypes.Stream {
	return sm.reservedStreams.getStreams()
}

// NumReservedStreams return the number of reserved streams.
func (sm *streamManager) NumReservedStreams() int {
	return sm.reservedStreams.size()
}

type (
	addStreamTask struct {
		st   sttypes.Stream
		errC chan error
	}

	rmStreamTask struct {
		id          sttypes.StreamID
		reason      string
		criticalErr bool
		errC        chan error
	}

	discTask struct{}

	stopTask struct {
		done chan struct{}
	}
)

// sanity checks the service, network and shard ID
func (sm *streamManager) sanityCheckStream(st sttypes.Stream) error {
	mySpec := sm.myProtoSpec
	rmSpec, err := st.ProtoSpec()
	if err != nil {
		return err
	}
	if sttypes.StreamID(sm.host.ID()) == st.ID() {
		return fmt.Errorf("can't connect to itself")
	}
	if mySpec.Service != rmSpec.Service {
		return fmt.Errorf("unexpected service: %v/%v", rmSpec.Service, mySpec.Service)
	}
	if mySpec.NetworkType != rmSpec.NetworkType {
		return fmt.Errorf("unexpected network: %v/%v", rmSpec.NetworkType, mySpec.NetworkType)
	}
	if mySpec.ShardID != rmSpec.ShardID {
		return fmt.Errorf("unexpected shard ID: %v/%v", rmSpec.ShardID, mySpec.ShardID)
	}
	return nil
}

func (sm *streamManager) handleAddStream(st sttypes.Stream) error {
	id := st.ID()
	// check if stream exists
	if _, ok := sm.streams.get(id); ok {
		return ErrStreamAlreadyExist
	}
	if _, ok := sm.reservedStreams.get(id); ok {
		return ErrStreamAlreadyExist
	}
	// Check if stream was recently removed
	if removalInfo, exists := sm.removedStreams.Get(id); exists {
		if !removalInfo.HasExpired() {
			return ErrStreamRemovalNotExpired
		}
	}

	// If the stream list has sufficient capacity, the stream can be added to the reserved list
	if sm.streams.size() >= sm.config.HiCap {
		if sm.reservedStreams.size() < MaxReservedStreams {
			if _, ok := sm.reservedStreams.get(id); !ok {
				sm.reservedStreams.addStream(st)
				sm.logger.Info().
					Int("NumStreams", sm.streams.size()).
					Int("NumReservedStreams", sm.reservedStreams.size()).
					Interface("StreamID", id).
					Msg("[StreamManager] added new stream to reserved list")
				numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))
			}
			return nil
		}
		return ErrTooManyStreams
	}

	sm.streams.addStream(st)
	sm.logger.Info().
		Int("NumStreams", sm.streams.size()).
		Interface("StreamID", id).
		Msg("[StreamManager] added new stream to main streams list")

	sm.addStreamFeed.Send(EvtStreamAdded{st})
	addedStreamsCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
	numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))

	// Update trusted peer metrics if this is a trusted peer
	trustedPeers := sm.getTrustedPeersMap()
	if _, trusted := trustedPeers[libp2p_peer.ID(id)]; trusted {
		trustedPeerStreamsAddedCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
		numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))
	}
	return nil
}

func (sm *streamManager) addStreamFromReserved(count int) (int, error) {
	if sm.reservedStreams.size() == 0 {
		return 0, errors.New("reserved streams list is empty")
	}
	added := 0
	for added < count && sm.reservedStreams.size() > 0 {
		st, err := sm.reservedStreams.popStream()
		if err != nil {
			return added, err
		}
		sm.streams.addStream(st)
		sm.logger.Info().
			Int("NumStreams", sm.streams.size()).
			Interface("StreamID", st.ID()).
			Msg("[StreamManager] added new stream from reserved streams list")
		sm.addStreamFeed.Send(EvtStreamAdded{st})
		addedStreamsCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
		numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))
		numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))

		// Update trusted peer metrics if this is a trusted peer
		trustedPeers := sm.getTrustedPeersMap()
		if _, trusted := trustedPeers[libp2p_peer.ID(st.ID())]; trusted {
			trustedPeerStreamsAddedCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
			numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))
		}
		added++
	}
	return added, nil
}

func (sm *streamManager) handleRemoveStream(id sttypes.StreamID, reason string, criticalErr bool) error {
	st, ok := sm.streams.get(id)
	if !ok {
		return ErrStreamAlreadyRemoved
	}

	if criticalErr {
		streamCriticalErrorCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
	}

	trustedPeers := sm.getTrustedPeersMap()
	if _, trusted := trustedPeers[libp2p_peer.ID(id)]; trusted {
		sm.logger.Info().
			Int("NumStreams", sm.streams.size()).
			Interface("StreamID", id).
			Bool("trusted", true).
			Str("reason", reason).
			Bool("criticalErr", criticalErr).
			Msg("[StreamManager] trusted peer got critical error but not removed")
		return nil
	}

	sm.streams.deleteStream(st)
	sm.reservedStreams.deleteStream(st)

	sm.logger.Info().
		Int("NumStreams", sm.streams.size()).
		Interface("StreamID", id).
		Str("reason", reason).
		Bool("criticalErr", criticalErr).
		Msg("[StreamManager] removed stream from main streams list")

	info, exist := sm.removedStreams.Get(id)
	if !exist {
		info = &RemovalInfo{count: 0}
		sm.removedStreams.Set(id, info)
	}
	info.MarkAsRemoved(criticalErr)

	// try to replace removed streams from reserved list
	sm.removeStreamFeed.Send(EvtStreamRemoved{id})
	removedStreamsCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
	numStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.streams.size()))
	numReservedStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.reservedStreams.size()))
	streamRemovalReasonCounterVec.With(prometheus.Labels{"reason": reason, "critical": strconv.FormatBool(criticalErr)}).Inc()
	numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))

	sm.tryToReplaceRemovedStream()

	return nil
}

func (sm *streamManager) tryToReplaceRemovedStream() error {
	// try to replace removed streams from the reserved list.
	requiredStreams := sm.hardRequiredStreams()

	if added, err := sm.addStreamFromReserved(requiredStreams); added > 0 {
		sm.logger.Info().
			Err(err). // in case if some new streams added and others failed
			Int("requiredStreams", requiredStreams).
			Int("added", added).
			Msg("added new streams from reserved list")
	}

	// if stream number is smaller than HardLoCap, spin up the discover
	if !sm.hardHaveEnoughStream() {
		select {
		case sm.discCh <- discTask{}:
		default:
		}
	}

	return nil
}

func (sm *streamManager) removeAllStreamOnClose() {
	var wg sync.WaitGroup

	for _, st := range sm.streams.slice() {
		wg.Add(1)
		go func(st sttypes.Stream) {
			defer wg.Done()
			err := st.CloseOnExit()
			if err != nil {
				sm.logger.Warn().Err(err).
					Interface("stream ID", st.ID()).
					Msg("failed to close stream")
			}
		}(st)
	}
	wg.Wait()

	// Be nice. after close, the field is still accessible to prevent potential panics
	sm.streams.Erase()
}

// waitForTrustedPeersInitialization waits for trusted peers to be initialized before starting discovery.
// It polls the host's TrustedPeersInitiated() function until it returns true, ensuring trusted peers
// are available for the first bootstrap discovery. The stream manager waits indefinitely (until context
// cancellation) for the host to complete initialization, as the host manages its own timeout.
func (sm *streamManager) waitForTrustedPeersInitialization() {
	if sm.trustedPeersInitiated == nil {
		return // No function provided, nothing to wait for
	}

	sm.logger.Info().Msg("[StreamManager] waiting for trusted peers initialization before starting discovery")

	// Check immediately first (host may have already completed initialization)
	if sm.trustedPeersInitiated() {
		sm.logger.Info().Msg("[StreamManager] trusted peers already initialized, proceeding with discovery")
		return
	}

	// Poll for trusted peers initialization to complete
	// The host manages its own timeout, so we wait indefinitely until host signals completion
	ticker := time.NewTicker(trustedPeersCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if sm.trustedPeersInitiated() {
				sm.logger.Info().Msg("[StreamManager] trusted peers initialization complete, proceeding with discovery")
				return
			}
		case <-sm.ctx.Done():
			sm.logger.Warn().Msg("[StreamManager] context cancelled while waiting for trusted peers")
			return
		}
	}
}

// getTrustedPeersMap returns the current map of trusted peers, or empty map if nil
func (sm *streamManager) getTrustedPeersMap() map[libp2p_peer.ID]struct{} {
	if sm.getTrustedPeers == nil {
		return make(map[libp2p_peer.ID]struct{})
	}
	return sm.getTrustedPeers()
}

// countTrustedPeerStreams counts the number of currently connected trusted peer streams
// This only counts streams in the main streams list, not reserved streams
func (sm *streamManager) countTrustedPeerStreams() int {
	trustedPeers := sm.getTrustedPeersMap()
	if len(trustedPeers) == 0 {
		return 0
	}
	count := 0
	for _, st := range sm.streams.getStreams() {
		if _, trusted := trustedPeers[libp2p_peer.ID(st.ID())]; trusted {
			count++
		}
	}
	return count
}

func (sm *streamManager) discoverAndSetupStream(discCtx context.Context) (int, error) {
	connecting := 0

	// Process trusted peers only once during bootstrap discovery.
	// After bootstrap, trusted peers will be handled through normal discovery if they disconnect.
	// This prevents aggressive retries of failed trusted peer connections on every discovery cycle.
	if !sm.trustedPeersProcessed.IsSet() && sm.getTrustedPeers != nil {
		trustedPeers := sm.getTrustedPeersMap()
		trustedPeerCount := len(trustedPeers)
		if trustedPeerCount > 0 {
			sm.trustedPeersProcessed.Set()
			sm.logger.Info().
				Int("trustedPeerCount", trustedPeerCount).
				Msg("[discoverAndSetupStream] processing trusted peers for bootstrap stream setup")

			for pid := range trustedPeers {
				if pid == sm.host.ID() {
					sm.logger.Debug().
						Interface("peerID", pid).
						Msg("[discoverAndSetupStream] skipping trusted peer (self)")
					continue
				}
				if sm.coolDownCache.Has(pid) {
					sm.logger.Debug().
						Interface("peerID", pid).
						Msg("[discoverAndSetupStream] skipping trusted peer (in cooldown)")
					continue
				}
				newStreamID := sttypes.StreamID(pid)
				if _, ok := sm.streams.get(newStreamID); ok {
					sm.logger.Debug().
						Interface("peerID", pid).
						Msg("[discoverAndSetupStream] skipping trusted peer (stream already exists)")
					continue
				}
				if _, ok := sm.reservedStreams.get(newStreamID); ok {
					sm.logger.Debug().
						Interface("peerID", pid).
						Msg("[discoverAndSetupStream] skipping trusted peer (in reserved streams)")
					continue
				}
				// Check if stream was recently removed
				if removalInfo, exists := sm.removedStreams.Get(newStreamID); exists {
					if !removalInfo.HasExpired() {
						sm.logger.Debug().
							Interface("peerID", pid).
							Time("removedAt", removalInfo.RemovedAt()).
							Msg("[discoverAndSetupStream] skipping trusted peer (removal cooldown not expired)")
						continue
					}
				}
				sm.logger.Info().
					Interface("peerID", pid).
					Msg("[discoverAndSetupStream] attempting to setup stream with trusted peer")
				discoveredPeersCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
				trustedPeerStreamsSetupAttemptsCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
				connecting += 1
				sm.setupSem <- struct{}{}
				go func(pid libp2p_peer.ID) {
					defer func() { <-sm.setupSem }()
					// The ctx here is using the module context instead of discover context
					err := sm.setupStreamWithPeer(sm.ctx, pid)
					if err != nil {
						sm.coolDownCache.Add(pid)
						trustedPeerStreamsConnectFailuresCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
						sm.logger.Warn().Err(err).
							Interface("peerID", pid).
							Msg("[discoverAndSetupStream] failed to setup stream with trusted peer")
						return
					}

					trustedPeerStreamsAddedCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
					numTrustedPeerStreamsGaugeVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Set(float64(sm.countTrustedPeerStreams()))

					sm.logger.Info().
						Interface("peerID", pid).
						Msg("[discoverAndSetupStream] new stream set up with trusted peer")
				}(pid)
			}
		}
	}

	if sm.streams.size()+connecting >= sm.config.HardLoCap {
		return connecting, nil
	}

	peers, err := sm.discover(discCtx)
	if err != nil {
		return connecting, errors.Wrap(err, "failed to discover")
	}
	discoverCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()

	for peer := range peers {
		if peer.ID == sm.host.ID() {
			continue
		}
		if sm.coolDownCache.Has(peer.ID) {
			continue
		}
		if _, ok := sm.streams.get(sttypes.StreamID(peer.ID)); ok {
			continue
		}
		if _, ok := sm.reservedStreams.get(sttypes.StreamID(peer.ID)); ok {
			continue
		}
		discoveredPeersCounterVec.With(prometheus.Labels{"topic": string(sm.myProtoID)}).Inc()
		connecting += 1
		go func(pid libp2p_peer.ID) {
			err := sm.setupStreamWithPeer(sm.ctx, pid)
			if err != nil {
				sm.coolDownCache.Add(pid)
				sm.logger.Warn().Err(err).
					Interface("peerID", pid).
					Msg("failed to setup stream with peer")
				return
			}
			sm.logger.Info().
				Interface("peerID", pid).
				Msg("new stream set up with peer")
		}(peer.ID)
	}

	return connecting, nil
}

func (sm *streamManager) discover(ctx context.Context) (<-chan libp2p_peer.AddrInfo, error) {
	numStreams := sm.streams.size()

	discBatch := sm.config.DiscBatch
	if sm.config.HiCap-numStreams < sm.config.DiscBatch {
		discBatch = sm.config.HiCap - numStreams
	}
	sm.logger.Debug().
		Interface("protoID", sm.myProtoID).
		Int("numStreams", numStreams).
		Int("discBatch", discBatch).
		Msg("[StreamManager] discovering")
	if discBatch < 0 {
		return nil, nil
	}

	ctx2, cancel := context.WithTimeout(ctx, discTimeout)
	go func() { // avoid context leak
		<-time.After(discTimeout)
		cancel()
	}()
	return sm.pf.FindPeers(ctx2, string(sm.myProtoID), discBatch)
}

func (sm *streamManager) setupStreamWithPeer(ctx context.Context, pid libp2p_peer.ID) error {
	timer := prometheus.NewTimer(setupStreamDuration.With(prometheus.Labels{"topic": string(sm.myProtoID)}))
	defer timer.ObserveDuration()

	nCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	st, err := sm.host.NewStream(nCtx, pid, protocol.ID(sm.myProtoID))
	if err != nil {
		return err
	}
	if sm.handleStream != nil {
		go sm.handleStream(st)
	}
	return nil
}

func (sm *streamManager) softHaveEnoughStreams() bool {
	availStreams := sm.streams.numStreamsWithMinProtoSpec(sm.myProtoSpec)
	return availStreams >= sm.config.SoftLoCap
}

func (sm *streamManager) hardHaveEnoughStream() bool {
	availStreams := sm.streams.numStreamsWithMinProtoSpec(sm.myProtoSpec)
	return availStreams >= sm.config.HardLoCap
}

func (sm *streamManager) hardRequiredStreams() int {
	availStreams := sm.streams.numStreamsWithMinProtoSpec(sm.myProtoSpec)
	if availStreams >= sm.config.HardLoCap {
		return 0
	}
	return sm.config.HardLoCap - availStreams
}
