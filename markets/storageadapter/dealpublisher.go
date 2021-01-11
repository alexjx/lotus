package storageadapter

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/node/config"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/api"

	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/actors/builtin/market"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
	market2 "github.com/filecoin-project/specs-actors/v2/actors/builtin/market"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"
)

type dealPublisherAPI interface {
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (miner.MinerInfo, error)
	MpoolPushMessage(ctx context.Context, msg *types.Message, spec *api.MessageSendSpec) (*types.SignedMessage, error)
}

// DealPublisher batches deal publishing so that many deals can be included in
// a single publish message. This saves gas for miners that publish deals
// frequently.
// When a deal is submitted, the DealPublisher waits a configurable amount of
// time for other deals to be submitted before sending the publish message.
// There is a configurable maximum number of deals that can be included in one
// message. When the limit is reached the DealPublisher immediately submits a
// publish message with all deals in the queue.
type DealPublisher struct {
	api dealPublisherAPI

	ctx      context.Context
	Shutdown context.CancelFunc

	maxDealsPerPublishMsg uint64
	publishPeriod         time.Duration
	publishSpec           *api.MessageSendSpec

	lk                     sync.Mutex
	pending                []*pendingDeal
	cancelWaitForMoreDeals context.CancelFunc
}

func NewDealPublisher(
	feeConfig *config.MinerFeeConfig,
	publishMsgCfg *config.PublishMsgConfig,
) func(lc fx.Lifecycle, dpapi dealPublisherAPI) *DealPublisher {
	return func(lc fx.Lifecycle, dpapi dealPublisherAPI) *DealPublisher {
		publishSpec := &api.MessageSendSpec{MaxFee: abi.TokenAmount(feeConfig.MaxPublishDealsFee)}
		dp := newDealPublisher(dpapi, publishMsgCfg, publishSpec)
		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				dp.Shutdown()
				return nil
			},
		})
		return dp
	}
}

func newDealPublisher(
	dpapi dealPublisherAPI,
	publishMsgCfg *config.PublishMsgConfig,
	publishSpec *api.MessageSendSpec,
) *DealPublisher {
	ctx, cancel := context.WithCancel(context.Background())
	return &DealPublisher{
		api:                   dpapi,
		ctx:                   ctx,
		Shutdown:              cancel,
		maxDealsPerPublishMsg: publishMsgCfg.MaxDealsPerMsg,
		publishPeriod:         time.Duration(publishMsgCfg.PublishPeriod),
		publishSpec:           publishSpec,
	}
}

func (p *DealPublisher) Publish(ctx context.Context, deal market2.ClientDealProposal) (cid.Cid, error) {
	pdeal := newPendingDeal(ctx, deal)

	// Add the deal to the queue
	p.processNewDeal(pdeal)

	// Wait for the deal to be submitted
	select {
	case <-ctx.Done():
		return cid.Undef, ctx.Err()
	case res := <-pdeal.Result:
		return res.msgCid, res.err
	}
}

func (p *DealPublisher) processNewDeal(pdeal *pendingDeal) {
	p.lk.Lock()
	defer p.lk.Unlock()

	// Filter out any cancelled deals
	p.filterCancelledDeals()

	// If all deals have been cancelled, clear the wait-for-deals timer
	if len(p.pending) == 0 && p.cancelWaitForMoreDeals != nil {
		p.cancelWaitForMoreDeals()
		p.cancelWaitForMoreDeals = nil
	}

	// Make sure the new deal hasn't been cancelled
	if pdeal.ctx.Err() != nil {
		return
	}

	// Add the new deal to the queue
	p.pending = append(p.pending, pdeal)

	// If the maximum number of deals per message has been reached,
	// send a publish message
	if uint64(len(p.pending)) >= p.maxDealsPerPublishMsg {
		p.publishAllDeals()
		return
	}

	// Otherwise wait for more deals to arrive or the timeout to be reached
	p.waitForMoreDeals()
}

func (p *DealPublisher) waitForMoreDeals() {
	// Check if the wait-for-deals timeout was already set
	if p.cancelWaitForMoreDeals != nil {
		return
	}

	// Set a timeout to wait for more deals to arrive
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancelWaitForMoreDeals = cancel

	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(p.publishPeriod):
			p.lk.Lock()
			defer p.lk.Unlock()

			// The timeout has expired so publish all pending deals
			p.publishAllDeals()
		}
	}()
}

func (p *DealPublisher) publishAllDeals() {
	// If the timeout hasn't yet been cancelled, cancel it
	if p.cancelWaitForMoreDeals != nil {
		p.cancelWaitForMoreDeals()
		p.cancelWaitForMoreDeals = nil
	}

	// Filter out any deals that have been cancelled
	p.filterCancelledDeals()
	deals := p.pending[:]
	p.pending = nil

	// Send the publish message
	go p.publishReady(deals)
}

func (p *DealPublisher) publishReady(ready []*pendingDeal) {
	if len(ready) == 0 {
		return
	}

	deals := make([]market2.ClientDealProposal, 0, len(ready))
	for _, pd := range ready {
		deals = append(deals, pd.deal)
	}

	// Send the publish message
	msgCid, err := p.publishDealProposals(deals)

	// Signal that each deal has been published
	for _, pd := range ready {
		pd := pd
		go func() {
			res := publishResult{
				msgCid: msgCid,
				err:    err,
			}
			select {
			case <-p.ctx.Done():
			case pd.Result <- res:
			}
		}()
	}
}

// Sends the publish message
func (p *DealPublisher) publishDealProposals(deals []market2.ClientDealProposal) (cid.Cid, error) {
	log.Infof("publishing %d deals with piece CIDs: %s", len(deals), pieceCids(deals))

	provider := deals[0].Proposal.Provider
	mi, err := p.api.StateMinerInfo(p.ctx, provider, types.EmptyTSK)
	if err != nil {
		return cid.Undef, err
	}

	params, err := actors.SerializeParams(&market2.PublishStorageDealsParams{
		Deals: deals,
	})

	if err != nil {
		return cid.Undef, xerrors.Errorf("serializing PublishStorageDeals params failed: %w", err)
	}

	smsg, err := p.api.MpoolPushMessage(p.ctx, &types.Message{
		To:     market.Address,
		From:   mi.Worker,
		Value:  types.NewInt(0),
		Method: market.Methods.PublishStorageDeals,
		Params: params,
	}, p.publishSpec)

	if err != nil {
		return cid.Undef, err
	}
	return smsg.Cid(), nil
}

func pieceCids(deals []market2.ClientDealProposal) string {
	cids := make([]string, 0, len(deals))
	for _, dl := range deals {
		cids = append(cids, dl.Proposal.PieceCID.String())
	}
	return strings.Join(cids, ", ")
}

// filter out deals that have been cancelled
func (p *DealPublisher) filterCancelledDeals() {
	i := 0
	for _, pd := range p.pending {
		if pd.ctx.Err() == nil {
			p.pending[i] = pd
			i++
		}
	}
	p.pending = p.pending[:i]
}

type publishResult struct {
	msgCid cid.Cid
	err    error
}

type pendingDeal struct {
	ctx    context.Context
	deal   market2.ClientDealProposal
	Result chan publishResult
}

func newPendingDeal(ctx context.Context, deal market2.ClientDealProposal) *pendingDeal {
	return &pendingDeal{
		ctx:    ctx,
		deal:   deal,
		Result: make(chan publishResult),
	}
}
