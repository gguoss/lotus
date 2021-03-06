package deals

import (
	"context"
	"math"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	cbor "github.com/ipfs/go-ipld-cbor"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/host"
	inet "github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/address"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/wallet"
	"github.com/filecoin-project/lotus/lib/cborrpc"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/retrieval/discovery"
)

func init() {
	cbor.RegisterCborType(ClientDeal{})
	cbor.RegisterCborType(actors.PieceInclVoucherData{}) // TODO: USE CBORGEN!
	cbor.RegisterCborType(types.SignedVoucher{})
	cbor.RegisterCborType(types.ModVerifyParams{})
	cbor.RegisterCborType(types.Signature{})
	cbor.RegisterCborType(actors.PaymentInfo{})
	cbor.RegisterCborType(api.PaymentInfo{})
	cbor.RegisterCborType(actors.InclusionProof{})
}

var log = logging.Logger("deals")

type ClientDeal struct {
	ProposalCid cid.Cid
	Proposal    StorageDealProposal
	State       api.DealState
	Miner       peer.ID

	s inet.Stream
}

type Client struct {
	sm        *stmgr.StateManager
	h         host.Host
	w         *wallet.Wallet
	dag       dtypes.ClientDAG
	discovery *discovery.Local

	deals ClientStateStore
	conns map[cid.Cid]inet.Stream

	incoming chan ClientDeal
	updated  chan clientDealUpdate

	stop    chan struct{}
	stopped chan struct{}
}

type clientDealUpdate struct {
	newState api.DealState
	id       cid.Cid
	err      error
}

func NewClient(sm *stmgr.StateManager, h host.Host, w *wallet.Wallet, ds dtypes.MetadataDS, dag dtypes.ClientDAG, discovery *discovery.Local) *Client {
	c := &Client{
		sm:        sm,
		h:         h,
		w:         w,
		dag:       dag,
		discovery: discovery,

		deals: ClientStateStore{StateStore{ds: namespace.Wrap(ds, datastore.NewKey("/deals/client"))}},
		conns: map[cid.Cid]inet.Stream{},

		incoming: make(chan ClientDeal, 16),
		updated:  make(chan clientDealUpdate, 16),

		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	return c
}

func (c *Client) Run(ctx context.Context) {
	go func() {
		defer close(c.stopped)

		for {
			select {
			case deal := <-c.incoming:
				c.onIncoming(deal)
			case update := <-c.updated:
				c.onUpdated(ctx, update)
			case <-c.stop:
				return
			}
		}
	}()
}

func (c *Client) onIncoming(deal ClientDeal) {
	log.Info("incoming deal")

	if _, ok := c.conns[deal.ProposalCid]; ok {
		log.Errorf("tracking deal connection: already tracking connection for deal %s", deal.ProposalCid)
		return
	}
	c.conns[deal.ProposalCid] = deal.s

	if err := c.deals.Begin(deal.ProposalCid, deal); err != nil {
		// We may have re-sent the proposal
		log.Errorf("deal tracking failed: %s", err)
		c.failDeal(deal.ProposalCid, err)
		return
	}

	go func() {
		c.updated <- clientDealUpdate{
			newState: api.DealUnknown,
			id:       deal.ProposalCid,
			err:      nil,
		}
	}()
}

func (c *Client) onUpdated(ctx context.Context, update clientDealUpdate) {
	log.Infof("Deal %s updated state to %d", update.id, update.newState)
	var deal ClientDeal
	err := c.deals.MutateClient(update.id, func(d *ClientDeal) error {
		d.State = update.newState
		deal = *d
		return nil
	})
	if update.err != nil {
		log.Errorf("deal %s failed: %s", update.id, update.err)
		c.failDeal(update.id, update.err)
		return
	}
	if err != nil {
		c.failDeal(update.id, err)
		return
	}

	switch update.newState {
	case api.DealUnknown: // new
		c.handle(ctx, deal, c.new, api.DealAccepted)
	case api.DealAccepted:
		c.handle(ctx, deal, c.accepted, api.DealStaged)
	case api.DealStaged:
		c.handle(ctx, deal, c.staged, api.DealSealing)
	case api.DealSealing:
		c.handle(ctx, deal, c.sealing, api.DealComplete)
	}
}

type ClientDealProposal struct {
	Data cid.Cid

	TotalPrice types.BigInt
	Duration   uint64

	Payment actors.PaymentInfo

	MinerAddress  address.Address
	ClientAddress address.Address
	MinerID       peer.ID
}

func (c *Client) VerifyParams(ctx context.Context, data cid.Cid) (*actors.PieceInclVoucherData, error) {
	commP, size, err := c.commP(ctx, data)
	if err != nil {
		return nil, err
	}

	return &actors.PieceInclVoucherData{
		CommP:     commP,
		PieceSize: types.NewInt(uint64(size)),
	}, nil
}

func (c *Client) Start(ctx context.Context, p ClientDealProposal, vd *actors.PieceInclVoucherData) (cid.Cid, error) {
	proposal := StorageDealProposal{
		PieceRef:          p.Data,
		SerializationMode: SerializationUnixFs,
		CommP:             vd.CommP[:],
		Size:              vd.PieceSize.Uint64(),
		TotalPrice:        p.TotalPrice,
		Duration:          p.Duration,
		Payment:           p.Payment,
		MinerAddress:      p.MinerAddress,
		ClientAddress:     p.ClientAddress,
	}

	s, err := c.h.NewStream(ctx, p.MinerID, ProtocolID)
	if err != nil {
		return cid.Undef, err
	}

	if err := c.sendProposal(s, proposal, p.ClientAddress); err != nil {
		return cid.Undef, err
	}

	proposalNd, err := cbor.WrapObject(proposal, math.MaxUint64, -1)
	if err != nil {
		return cid.Undef, err
	}

	deal := ClientDeal{
		ProposalCid: proposalNd.Cid(),
		Proposal:    proposal,
		State:       api.DealUnknown,
		Miner:       p.MinerID,

		s: s,
	}

	// TODO: actually care about what happens with the deal after it was accepted
	c.incoming <- deal

	// TODO: start tracking after the deal is sealed
	return deal.ProposalCid, c.discovery.AddPeer(p.Data, discovery.RetrievalPeer{
		Address: proposal.MinerAddress,
		ID:      deal.Miner,
	})
}

func (c *Client) QueryAsk(ctx context.Context, p peer.ID, a address.Address) (*types.SignedStorageAsk, error) {
	s, err := c.h.NewStream(ctx, p, AskProtocolID)
	if err != nil {
		return nil, err
	}

	req := &AskRequest{
		Miner: a,
	}
	if err := cborrpc.WriteCborRPC(s, req); err != nil {
		return nil, xerrors.Errorf("failed to send ask request: %w", err)
	}

	var out AskResponse
	if err := cborrpc.ReadCborRPC(s, &out); err != nil {
		return nil, xerrors.Errorf("failed to read ask response: %w", err)
	}

	if out.Ask == nil {
		return nil, xerrors.Errorf("got no ask back")
	}

	if out.Ask.Ask.Miner != a {
		return nil, xerrors.Errorf("got back ask for wrong miner")
	}

	if err := c.checkAskSignature(out.Ask); err != nil {
		return nil, xerrors.Errorf("ask was not properly signed")
	}

	return out.Ask, nil
}

func (c *Client) List() ([]ClientDeal, error) {
	return c.deals.ListClient()
}

func (c *Client) Stop() {
	close(c.stop)
	<-c.stopped
}
