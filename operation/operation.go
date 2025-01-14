package operation

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/doublecloud/go-genproto/doublecloud/clickhouse/v1"
	"github.com/doublecloud/go-genproto/doublecloud/kafka/v1"
	"github.com/doublecloud/go-genproto/doublecloud/network/v1"
	"github.com/doublecloud/go-genproto/doublecloud/transfer/v1"
	"github.com/doublecloud/go-genproto/doublecloud/v1"
	dc "github.com/doublecloud/go-genproto/doublecloud/v1"
	"github.com/doublecloud/go-sdk/pkg/sdkerrors"
)

const (
	CLICKHOUSE_OPERATION_PREFIX         = "cho"
	KAFKA_OPERATION_PREFIX              = "kfo"
	TRANSFER_OPERATION_PREFIX           = "dtj"
	TRANSFER_ENDPOINTS_OPERATION_PREFIX = "dte"
)

var _ = emptypb.Empty{}

type OperationServiceClient interface {
}

type Client = OperationServiceClient

type Proto = dc.Operation

func New(client Client, proto *Proto) *Operation {
	if proto == nil {
		panic("nil operation")
	}
	return &Operation{proto: proto, client: client, newTimer: defaultTimer}
}

func defaultTimer(d time.Duration) (func() <-chan time.Time, func() bool) {
	timer := time.NewTimer(d)
	return func() <-chan time.Time {
		return timer.C
	}, timer.Stop
}

type Operation struct {
	proto    *Proto
	client   Client
	newTimer func(time.Duration) (func() <-chan time.Time, func() bool)
}

func (o *Operation) Proto() *Proto  { return o.proto }
func (o *Operation) Client() Client { return o.client }

//revive:disable:var-naming
func (o *Operation) Id() string { return o.proto.GetId() }

//revive:enable:var-naming
func (o *Operation) Description() string { return o.proto.GetDescription() }
func (o *Operation) CreatedBy() string   { return o.proto.GetCreatedBy() }

func (o *Operation) ResourceId() string { return o.proto.GetResourceId() }

func (o *Operation) CreatedAt() time.Time {
	return o.proto.GetCreateTime().AsTime()
}

func (o *Operation) Metadata() map[string]string {
	return o.proto.GetMetadata()
}

func (o *Operation) Error() error {
	st := o.ErrorStatus()
	if st == nil {
		return nil
	}
	return st.Err()
}

func (o *Operation) ErrorStatus() *status.Status {
	proto := o.proto.GetError()
	if proto == nil {
		return nil
	}
	return status.FromProto(proto)
}

func (o *Operation) Done() bool {
	return o.proto.GetStatus() == dc.Operation_STATUS_DONE || o.proto.GetStatus() == dc.Operation_STATUS_INVALID
}
func (o *Operation) Ok() bool     { return o.Done() && o.proto.GetError() == nil }
func (o *Operation) Failed() bool { return o.Done() && o.proto.GetError() != nil }

// Poll gets new state of operation from operation client. On success the operation state is updated.
// Returns error if update request failed.
func (o *Operation) Poll(ctx context.Context, opts ...grpc.CallOption) error {
	var state *doublecloud.Operation
	var err error

	if strings.HasPrefix(o.Id(), CLICKHOUSE_OPERATION_PREFIX) {
		state, err = o.Client().(clickhouse.OperationServiceClient).Get(ctx, &clickhouse.GetOperationRequest{OperationId: o.Id()}, opts...)
	} else if strings.HasPrefix(o.Id(), KAFKA_OPERATION_PREFIX) {
		state, err = o.Client().(kafka.OperationServiceClient).Get(ctx, &kafka.GetOperationRequest{OperationId: o.Id()}, opts...)
	} else if strings.HasPrefix(o.Id(), TRANSFER_OPERATION_PREFIX) || strings.HasPrefix(o.Id(), TRANSFER_ENDPOINTS_OPERATION_PREFIX) {
		state, err = o.Client().(transfer.OperationServiceClient).Get(ctx, &transfer.GetOperationRequest{OperationId: o.Id()}, opts...)
	} else if _, err := uuid.Parse(o.Id()); err == nil {
		state, err = o.Client().(network.OperationServiceClient).Get(ctx, &network.GetOperationRequest{OperationId: o.Id()}, opts...)
	}
	if state == nil {
		return sdkerrors.WithMessagef(err, "operation (id=%s) unknown type", o.Id())
	}
	if err != nil {
		return err
	}
	o.proto = state
	return nil
}

const DefaultPollInterval = time.Second

func (o *Operation) Wait(ctx context.Context, opts ...grpc.CallOption) error {
	return o.WaitInterval(ctx, DefaultPollInterval, opts...)
}

func (o *Operation) WaitInterval(ctx context.Context, pollInterval time.Duration, opts ...grpc.CallOption) error {
	return o.waitInterval(ctx, pollInterval, opts...)
}

const (
	pollIntervalMetadataKey = "x-operation-poll-interval"
)

func (o *Operation) waitInterval(ctx context.Context, pollInterval time.Duration, opts ...grpc.CallOption) error {
	var headers metadata.MD
	opts = append(opts, grpc.Header(&headers))

	// Sometimes, the returned operation is not on all replicas yet,
	// so we need to ignore first couple of NotFound errors.
	const maxNotFoundRetry = 3
	notFoundCount := 0
	for !o.Done() {
		headers = metadata.MD{}
		err := o.Poll(ctx, opts...)
		if err != nil {
			if notFoundCount < maxNotFoundRetry && shoudRetry(err) {
				notFoundCount++
			} else {
				// Message needed to distinguish poll fail and operation error, which are both gRPC status.
				return sdkerrors.WithMessagef(err, "operation (id=%s) poll fail", o.Id())
			}
		}
		if o.Done() {
			break
		}
		interval := pollInterval
		if vals := headers.Get(pollIntervalMetadataKey); len(vals) > 0 {
			i, err := strconv.Atoi(vals[0])
			if err == nil {
				interval = time.Duration(i) * time.Second
			}
		}
		if interval <= 0 {
			continue
		}
		wait, stop := o.newTimer(interval)
		select {
		case <-wait():
		case <-ctx.Done():
			stop()
			return sdkerrors.WithMessagef(ctx.Err(), "operation (id=%s) wait context done", o.Id())
		}
	}
	return sdkerrors.WithMessagef(o.Error(), "operation (id=%s) failed", o.Id())
}

func shoudRetry(err error) bool {
	status, ok := status.FromError(err)
	return ok && status.Code() == codes.NotFound
}

func unmarshalAny(msg *anypb.Any) (proto.Message, error) {
	if msg == nil {
		return nil, nil
	}
	return msg.UnmarshalNew()
}
