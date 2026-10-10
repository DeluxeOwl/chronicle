package queue_test

import (
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestQueue_PayloadFactoryMustMatchTheQueueType(t *testing.T) {
	for _, option := range []queue.Option{
		queue.WithPayloadFactory(func() *wrapperspb.StringValue { return new(wrapperspb.StringValue) }),
		queue.WithPayloadFactory(func() string { return "" }),
		queue.WithPayloadFactory[string](nil),
	} {
		q, err := queue.New[string](eventlog.NewMemory(), "messages", option)
		require.Error(t, err)
		require.Nil(t, q)
	}
}

func TestQueue_NilFactoryResultDoesNotConsumeTheMessage(t *testing.T) {
	log := eventlog.NewMemory()
	q, err := queue.New[*wrapperspb.StringValue](log, "messages",
		queue.WithCodec(encoding.NewProtobuf()),
		queue.WithPayloadFactory(func() *wrapperspb.StringValue { return nil }))
	require.NoError(t, err)
	require.NoError(t, q.Enqueue(t.Context(), queue.Message[*wrapperspb.StringValue]{
		ID: "welcome-123", Payload: wrapperspb.String("Welcome!"),
	}))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.Error(t, err)
	require.Nil(t, delivery)
	other, err := queue.New[*wrapperspb.StringValue](
		log,
		"messages",
		queue.WithCodec(encoding.NewProtobuf()),
		queue.WithPayloadFactory(
			func() *wrapperspb.StringValue { return new(wrapperspb.StringValue) },
		),
	)
	require.NoError(t, err)
	delivery, err = other.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, uint64(1), delivery.Attempt)
	require.Equal(t, "Welcome!", delivery.Message.Payload.GetValue())
}

func TestQueue_ProtobufRequiresAnExplicitPayloadFactory(t *testing.T) {
	q, err := queue.New[*wrapperspb.StringValue](
		eventlog.NewMemory(),
		"messages",
		queue.WithCodec(encoding.NewProtobuf()),
	)
	require.Error(
		t,
		err,
		"the queue must not assume every Protobuf message has a usable zero value",
	)
	require.Nil(t, q)
}

func TestQueue_DynamicProtobufPayloadUsesItsDescriptorAwareFactory(t *testing.T) {
	file, err := protodesc.NewFile(
		protodesc.ToFileDescriptorProto(wrapperspb.File_google_protobuf_wrappers_proto),
		nil,
	)
	require.NoError(t, err)
	descriptor := file.Messages().ByName("StringValue")
	q, err := queue.New[*dynamicpb.Message](
		eventlog.NewMemory(),
		"dynamic-messages",
		queue.WithCodec(encoding.NewProtobuf()),
		queue.WithPayloadFactory(
			func() *dynamicpb.Message { return dynamicpb.NewMessage(descriptor) },
		),
	)
	require.NoError(t, err)
	payload := dynamicpb.NewMessage(descriptor)
	payload.Set(descriptor.Fields().ByName("value"), protoreflect.ValueOfString("Welcome!"))
	require.NoError(
		t,
		q.Enqueue(
			t.Context(),
			queue.Message[*dynamicpb.Message]{ID: "welcome-123", Payload: payload},
		),
	)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(
		t,
		"Welcome!",
		delivery.Message.Payload.Get(descriptor.Fields().ByName("value")).String(),
	)
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))
}
