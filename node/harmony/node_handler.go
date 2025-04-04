package node

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/utils/crosslinks"

	"github.com/harmony-one/harmony/api/proto"
	proto_node "github.com/harmony-one/harmony/api/proto/node"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/staking/slash"
	staking "github.com/harmony-one/harmony/staking/types"
)

const p2pMsgPrefixSize = 5
const p2pNodeMsgPrefixSize = proto.MessageTypeBytes + proto.MessageCategoryBytes

// some messages have uninteresting fields in header, slash, receipt and crosslink are
// such messages. This function assumes that input bytes are a slice which already
// past those not relevant header bytes.
func (node *Node) processSkippedMsgTypeByteValue(
	cat proto_node.BlockMessageType, content []byte,
) {
	switch cat {
	case proto_node.SlashCandidate:
		node.processSlashCandidateMessage(content)
	case proto_node.Receipt:
		node.ProcessReceiptMessage(content)
	case proto_node.CrossLink:
		node.ProcessCrossLinkMessage(content)
	case proto_node.CrosslinkHeartbeat:
		node.ProcessCrossLinkHeartbeatMessage(content)
	case proto_node.Epoch:
		node.ProcessEpochBlockMessage(content)
	default:
		utils.Logger().Error().
			Int("message-iota-value", int(cat)).
			Msg("Invariant usage of processSkippedMsgTypeByteValue violated")
	}
}

// HandleNodeMessage parses the message and dispatch the actions.
func (node *Node) HandleNodeMessage(
	ctx context.Context,
	msgPayload []byte,
	actionType proto_node.MessageType,
) error {
	switch actionType {
	case proto_node.Transaction:
		node.transactionMessageHandler(msgPayload)
	case proto_node.Staking:
		node.stakingMessageHandler(msgPayload)
	case proto_node.Block:
		switch blockMsgType := proto_node.BlockMessageType(msgPayload[0]); blockMsgType {
		case proto_node.Sync:
			blocks := []*types.Block{}
			if err := rlp.DecodeBytes(msgPayload[1:], &blocks); err != nil {
				utils.Logger().Error().
					Err(err).
					Msg("block sync")
			} else {
				// for non-beaconchain node, subscribe to beacon block broadcast
				if node.Blockchain().ShardID() != shard.BeaconChainShardID {
					for _, block := range blocks {
						if block.ShardID() == 0 {
							if block.IsLastBlockInEpoch() {
								go func(blk *types.Block) {
									node.BeaconBlockChannel <- blk
								}(block)
							}
						}
					}
				}
			}
		case
			proto_node.SlashCandidate,
			proto_node.Receipt,
			proto_node.CrossLink,
			proto_node.CrosslinkHeartbeat,
			proto_node.Epoch:
			// skip first byte which is blockMsgType
			node.processSkippedMsgTypeByteValue(blockMsgType, msgPayload[1:])
		}
	default:
		utils.Logger().Error().
			Str("Unknown actionType", string(actionType))
	}
	return nil
}

func (node *Node) transactionMessageHandler(msgPayload []byte) {
	txMessageType := proto_node.TransactionMessageType(msgPayload[0])

	switch txMessageType {
	case proto_node.Send:
		txs := types.Transactions{}
		err := rlp.Decode(bytes.NewReader(msgPayload[1:]), &txs) // skip the Send messge type
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Msg("Failed to deserialize transaction list")
			return
		}
		addPendingTransactions(node.registry, txs)
	}
}

func (node *Node) stakingMessageHandler(msgPayload []byte) {
	txMessageType := proto_node.TransactionMessageType(msgPayload[0])

	switch txMessageType {
	case proto_node.Send:
		txs := staking.StakingTransactions{}
		err := rlp.Decode(bytes.NewReader(msgPayload[1:]), &txs) // skip the Send message type
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Msg("Failed to deserialize staking transaction list")
			return
		}
		node.addPendingStakingTransactions(txs)
	}
}

// BroadcastNewBlock is called by consensus leader to sync new blocks with other clients/nodes.
// NOTE: For now, just send to the client (basically not broadcasting)
// TODO (lc): broadcast the new blocks to new nodes doing state sync
func BroadcastNewBlock(host p2p.Host, newBlock *types.Block, nodeConfig *nodeconfig.ConfigType) {
	groups := []nodeconfig.GroupID{nodeConfig.GetClientGroupID()}
	utils.Logger().Info().
		Msgf(
			"broadcasting new block %d, group %s", newBlock.NumberU64(), groups[0],
		)
	msg := p2p.ConstructMessage(
		proto_node.ConstructBlocksSyncMessage([]*types.Block{newBlock}),
	)
	if err := host.SendMessageToGroups(groups, msg); err != nil {
		utils.Logger().Warn().Err(err).Msg("cannot broadcast new block")
	}
}

// BroadcastSlash ..
func (node *Node) BroadcastSlash(witness *slash.Record) {
	if err := node.host.SendMessageToGroups(
		[]nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID)},
		p2p.ConstructMessage(
			proto_node.ConstructSlashMessage(slash.Records{*witness})),
	); err != nil {
		utils.Logger().Err(err).
			RawJSON("record", []byte(witness.String())).
			Msg("could not send slash record to beaconchain")
	}
	utils.Logger().Info().Msg("broadcast the double sign record")
}

// BroadcastCrossLinkFromShardsToBeacon is called by consensus leader to
// send the new header as cross link to beacon chain.
func (node *Node) BroadcastCrossLinkFromShardsToBeacon() { // leader of 1-3 shards
	if node.IsRunningBeaconChain() {
		return
	}
	if !(node.Consensus.IsLeader() || rand.Intn(100) <= 1) {
		return
	}
	curBlock := node.Blockchain().CurrentBlock()
	if curBlock == nil {
		return
	}

	if !node.Blockchain().Config().IsCrossLink(curBlock.Epoch()) {
		// no need to broadcast crosslink if it's beacon chain, or it's not crosslink epoch
		return
	}

	utils.Logger().Info().Msgf(
		"Construct and Broadcasting new crosslink to beacon chain groupID %s",
		nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID),
	)

	headers, err := getCrosslinkHeadersForShards(node.Blockchain(), curBlock, node.crosslinks)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[BroadcastCrossLink] failed to get crosslinks")
		return
	}

	if len(headers) == 0 {
		utils.Logger().Info().Msg("[BroadcastCrossLink] no crosslinks to broadcast")
		return
	}

	for _, h := range headers {
		utils.Logger().Info().Msgf("[BroadcastCrossLink] header shard %d blockNum %d", h.ShardID(), h.Number().Uint64())
	}

	err = node.host.SendMessageToGroups(
		[]nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID)},
		p2p.ConstructMessage(
			proto_node.ConstructCrossLinkMessage(node.Consensus.Blockchain(), headers)),
	)
	if err != nil {
		utils.Logger().Error().Err(err).Msgf("[BroadcastCrossLink] failed to broadcast message")
	} else {
		node.crosslinks.SetLatestSentCrosslinkBlockNumber(headers[len(headers)-1].Number().Uint64())
	}
}

// BroadcastCrosslinkHeartbeatSignalFromBeaconToShards is called by consensus leader or 1% validators to
// send last cross link to shard chains.
func (node *Node) BroadcastCrosslinkHeartbeatSignalFromBeaconToShards() { // leader of 0 shard
	if !node.IsRunningBeaconChain() {
		return
	}
	if !(node.IsCurrentlyLeader() || rand.Intn(100) == 0) {
		return
	}

	curBlock := node.Beaconchain().CurrentBlock()
	if curBlock == nil {
		return
	}

	if !node.Blockchain().Config().IsCrossLink(curBlock.Epoch()) {
		// no need to broadcast crosslink if it's beacon chain, or it's not crosslink epoch
		return
	}

	var privToSign *bls.PrivateKeyWrapper
	for _, priv := range node.Consensus.GetPrivateKeys() {
		if node.Consensus.IsValidatorInCommittee(priv.Pub.Bytes) {
			privToSign = &priv
			break
		}
	}

	if privToSign == nil {
		return
	}
	instance := shard.Schedule.InstanceForEpoch(curBlock.Epoch())
	for shardID := uint32(1); shardID < instance.NumShards(); shardID++ {
		lastLink, err := node.Blockchain().ReadShardLastCrossLink(shardID)
		if err != nil {
			utils.Logger().Error().Err(err).Msg("[BroadcastCrossLinkSignal] failed to get crosslinks")
			continue
		} else if lastLink == nil {
			continue
		}
		hb := types.CrosslinkHeartbeat{
			ShardID:                  lastLink.ShardID(),
			LatestContinuousBlockNum: lastLink.BlockNum(),
			Epoch:                    lastLink.Epoch().Uint64(),
			PublicKey:                privToSign.Pub.Bytes[:],
			Signature:                nil,
		}

		rs, err := rlp.EncodeToBytes(hb)
		if err != nil {
			utils.Logger().Error().Err(err).Msg("[BroadcastCrossLinkSignal] failed to encode signal")
			continue
		}
		hb.Signature = privToSign.Pri.SignHash(rs).Serialize()
		bts := proto_node.ConstructCrossLinkHeartBeatMessage(hb)
		node.host.SendMessageToGroups(
			[]nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(shardID))},
			p2p.ConstructMessage(bts),
		)
	}
}

// getCrosslinkHeadersForShards get headers required for crosslink creation.
func getCrosslinkHeadersForShards(shardChain core.BlockChain, curBlock *types.Block, crosslinks *crosslinks.Crosslinks) ([]*block.Header, error) {
	var headers []*block.Header
	signal := crosslinks.LastKnownCrosslinkHeartbeatSignal()
	var latestBlockNum uint64

	if signal == nil {
		utils.Logger().Debug().Msg("[BroadcastCrossLink] no known crosslink heartbeat signal")
		header := shardChain.GetHeaderByNumber(curBlock.NumberU64() - 2)
		if header != nil && shardChain.Config().IsCrossLink(header.Epoch()) {
			headers = append(headers, header)
		}
		header = shardChain.GetHeaderByNumber(curBlock.NumberU64() - 1)
		if header != nil && shardChain.Config().IsCrossLink(header.Epoch()) {
			headers = append(headers, header)
		}
		return append(headers, curBlock.Header()), nil
	}
	latestBlockNum = signal.LatestContinuousBlockNum
	latest := crosslinks.LatestSentCrosslinkBlockNumber()
	if latest > latestBlockNum && latest <= latestBlockNum+crossLinkBatchSize*6 {
		latestBlockNum = latest
	}

	batchSize := crossLinkBatchSize
	diff := curBlock.Number().Uint64() - latestBlockNum

	// Increase batch size by 1 for every 5 blocks behind
	batchSize += int(diff) / 5

	// Cap at a sane size to avoid overload network
	if batchSize > crossLinkBatchSize*2 {
		batchSize = crossLinkBatchSize * 2
	}

	for blockNum := latestBlockNum + 1; blockNum <= curBlock.NumberU64(); blockNum++ {
		header := shardChain.GetHeaderByNumber(blockNum)
		if header != nil && shardChain.Config().IsCrossLink(header.Epoch()) {
			headers = append(headers, header)
			if len(headers) == batchSize {
				break
			}
		}
	}

	return headers, nil
}

// BootstrapConsensus is a goroutine to check number of peers and start the consensus
func (node *Node) BootstrapConsensus() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	min := node.Consensus.MinPeers
	enoughMinPeers := make(chan struct{}, 1)
	const checkEvery = 3 * time.Second
	go func() {
		for {
			<-time.After(checkEvery)
			numPeersNow := node.host.GetPeerCount()
			connectedPeers := len(node.host.Network().Peers())
			if connectedPeers >= min {
				utils.Logger().Info().Msg("[bootstrap] StartConsensus")
				enoughMinPeers <- struct{}{}
				fmt.Printf("Bootstrap consensus done. Connected %d, known %d, shard: %d\n", connectedPeers, numPeersNow, node.Consensus.ShardID)
				return
			}
			utils.Logger().Info().
				Int("numPeersNow", numPeersNow).
				Int("targetNumPeers", min).
				Dur("next-peer-count-check-in-seconds", checkEvery).
				Msg("do not have enough min peers yet in bootstrap of consensus")
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-enoughMinPeers:
		go func() {
			node.Consensus.StartChannel()
		}()
		return nil
	}
}
