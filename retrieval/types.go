package retrieval

import (
	"github.com/filecoin-project/lotus/api"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"

	"github.com/filecoin-project/lotus/chain/types"
)

const ProtocolID = "/fil/retrieval/-1.0.0"          // TODO: spec
const QueryProtocolID = "/fil/retrieval/qry/-1.0.0" // TODO: spec

type QueryResponseStatus int

const (
	Available QueryResponseStatus = iota
	Unavailable
)

const (
	Accepted = iota
	Error
	Rejected
	Unsealing
)

func init() {
	cbor.RegisterCborType(RetParams{})

	cbor.RegisterCborType(Query{})
	cbor.RegisterCborType(QueryResponse{})
	cbor.RegisterCborType(Unixfs0Offer{})

	cbor.RegisterCborType(DealProposal{})
	cbor.RegisterCborType(DealResponse{})
	cbor.RegisterCborType(Block{})
}

type Query struct {
	Piece cid.Cid
	// TODO: payment
}

type QueryResponse struct {
	Status QueryResponseStatus

	Size uint64 // TODO: spec
	// TODO: unseal price (+spec)
	// TODO: sectors to unseal
	// TODO: address to send money for the deal?
	MinPrice types.BigInt
}

type Unixfs0Offer struct {
	Offset uint64
	Size   uint64
}

type RetParams struct {
	Unixfs0 *Unixfs0Offer
}

type DealProposal struct {
	Payment api.PaymentInfo

	Ref    cid.Cid
	Params RetParams
}

type DealResponse struct {
	Status  int
	Message string
}

type Block struct { // TODO: put in spec
	Prefix []byte // TODO: fix cid.Prefix marshaling somehow
	Data   []byte
}
