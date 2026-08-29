// Package events carries seat state changes over Kafka.
//
// WHY THIS EXISTS AT ALL, since the system worked without it: the seat map was
// polled. Every browser asked for a whole section every two seconds, and each
// ask cost a gateway request plus a catalog call plus an inventory call — almost
// always to be told nothing had changed. At 96 cinema seats that is free. At
// 2,000 seats a section with an on-sale audience watching, it is the dominant
// read load on the system, and it grows with the size of the venue rather than
// with the number of things actually happening.
//
// Inventory publishes a change once. Everything that cares hears about it.
//
// AT-LEAST-ONCE, AND EVERY CONSUMER MUST BE IDEMPOTENT. A seat state message is
// naturally idempotent — it says "this seat is now sold", not "increment
// something" — which is why the payload is a STATE and not a delta. Assume every
// message arrives twice, because it will.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Topics. These names are also in DESIGN.md's EVENTS table and in the
// KafkaTopic CRs under deploy/data; changing one means changing all three.
const (
	TopicSeatHeld     = "inventory.seat.held"
	TopicSeatReleased = "inventory.seat.released"
	TopicSeatSold     = "inventory.seat.sold"

	// THE OTHER HALF OF THE ASYMMETRY, and the reason these exist at all.
	//
	// The inventory topics key by EVENT ID, so during an on-sale every message
	// for one concert lands on ONE partition and the load does not spread at all.
	// These key by ORDER ID, which spreads perfectly — orders are independent of
	// each other and nothing needs cross-order ordering.
	//
	// Same cluster, same on-sale, one set of topics idle and one partition on
	// fire. That contrast is what milestone 10 measures, and it only exists if
	// both halves are produced.
	TopicOrderCreated     = "orders.created"
	TopicOrderConfirmed   = "orders.confirmed"
	TopicOrderFailed      = "orders.failed"
	TopicPaymentSucceeded = "payments.succeeded"
	TopicPaymentFailed    = "payments.failed"
)

// OrderChange is what the orders.* and payments.* topics carry.
type OrderChange struct {
	OrderID     string    `json:"order_id"`
	EventID     string    `json:"event_id,omitempty"`
	HoldID      string    `json:"hold_id,omitempty"`
	UserID      string    `json:"user_id,omitempty"`
	AmountMinor int64     `json:"amount_minor,omitempty"`
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	At          time.Time `json:"at"`
}

// SeatChange is what every one of those topics carries.
//
// ONE SHAPE FOR ALL THREE rather than a type per topic: consumers overwhelmingly
// want "what is the status of this seat now", and the topic name is the only
// thing that differs. A reader that wants to treat them differently still can.
type SeatChange struct {
	EventID   string    `json:"event_id"`
	SectionID string    `json:"section_id,omitempty"`
	SeatIDs   []string  `json:"seat_ids"`
	HoldID    string    `json:"hold_id,omitempty"`
	OrderID   string    `json:"order_id,omitempty"`
	Status    string    `json:"status"` // available | held | sold
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// Publisher writes seat changes. Only inventory has one.
type Publisher struct {
	w  *kafka.Writer
	lg *zap.Logger
}

func NewPublisher(brokers []string, lg *zap.Logger) *Publisher {
	return &Publisher{
		lg: lg,
		w: &kafka.Writer{
			Addr: kafka.TCP(brokers...),
			// The topic is per-message, because one publisher writes to three.
			Balancer: &kafka.Hash{},
			// ASYNC, AND THIS IS THE IMPORTANT DECISION. A seat claim must not
			// wait on Kafka: the database transaction has already committed and
			// the customer is owed an answer. If the broker is slow or gone, the
			// sale continues and the read model goes stale — which is a bad day,
			// not a broken system. Making this synchronous would put a message
			// broker in the path of every purchase.
			Async:        true,
			BatchTimeout: 50 * time.Millisecond,
			RequiredAcks: kafka.RequireOne,
			Completion: func(msgs []kafka.Message, err error) {
				// Where every producer span ends. kafka-go fills Partition and
				// Offset into each message just before calling this, which is the
				// only place those are knowable for an async writer.
				finishPublish(msgs, err)

				if err != nil {
					// Logged, never returned. Nobody is waiting for this and there
					// is nothing useful to do about it in the request path.
					lg.Warn("seat change not published",
						zap.Int("messages", len(msgs)), zap.Error(err))
				}
			},
		},
	}
}

// Publish sends one change. It does not block on the broker.
//
// The KEY IS THE EVENT ID, deliberately, which is what DESIGN.md says and what
// makes the milestone 10 experiment interesting: every seat change for one
// concert lands on one partition, so an on-sale does not spread at all, while
// orders keyed by order_id spread perfectly. That asymmetry is the thing to go
// and measure, and it only exists if the key is right from the start.
func (p *Publisher) Publish(ctx context.Context, topic string, c SeatChange) {
	if p == nil || p.w == nil {
		return // no broker configured: the system runs without one
	}
	c.At = time.Now()
	body, err := json.Marshal(c)
	if err != nil {
		p.lg.Warn("seat change would not marshal", zap.Error(err))
		return
	}
	ctx, headers, ps := startPublish(ctx, topic, c.EventID, len(body))

	// The span is ENDED BY THE WRITER'S Completion CALLBACK, not here — that is
	// the first moment the partition is known. If the message never gets queued
	// at all, Completion will never see it, so it is ended here instead.
	if err := p.w.WriteMessages(ctx, kafka.Message{
		Topic:      topic,
		Key:        []byte(c.EventID),
		Value:      body,
		Headers:    headers,
		WriterData: ps,
	}); err != nil {
		p.lg.Warn("seat change not queued", zap.String("topic", topic), zap.Error(err))
		finishPublish([]kafka.Message{{WriterData: ps}}, err)
	}
}

// PublishOrder sends an order or payment event, KEYED BY ORDER ID.
//
// The key is the whole point — see the topic constants. Using the event id here
// would collapse the two halves of the experiment into one hot partition and
// there would be nothing left to compare.
func (p *Publisher) PublishOrder(ctx context.Context, topic string, c OrderChange) {
	if p == nil || p.w == nil {
		return
	}
	c.At = time.Now()
	body, err := json.Marshal(c)
	if err != nil {
		p.lg.Warn("order change would not marshal", zap.Error(err))
		return
	}
	ctx, headers, ps := startPublish(ctx, topic, c.OrderID, len(body))

	// The span is ENDED BY THE WRITER'S Completion CALLBACK, not here — that is
	// the first moment the partition is known. If the message never gets queued
	// at all, Completion will never see it, so it is ended here instead.
	if err := p.w.WriteMessages(ctx, kafka.Message{
		Topic:      topic,
		Key:        []byte(c.OrderID),
		Value:      body,
		Headers:    headers,
		WriterData: ps,
	}); err != nil {
		p.lg.Warn("order change not queued", zap.String("topic", topic), zap.Error(err))
		finishPublish([]kafka.Message{{WriterData: ps}}, err)
	}
}

func (p *Publisher) Close() error {
	if p == nil || p.w == nil {
		return nil
	}
	return p.w.Close()
}

// Subscribe reads every seat change and calls fn for each.
//
// EACH READER GETS ITS OWN GROUP ID, WHICH IS THE OPPOSITE OF THE USUAL ADVICE
// and is essential here. A consumer group SHARES the partitions between members,
// so with six gateway replicas in one group each message would reach exactly one
// of them — and a browser connected to any of the other five would never hear
// about it. This is a broadcast, not a work queue: every replica needs every
// message. The group id therefore includes something unique per process.
// fn RECEIVES THE MESSAGE'S CONTEXT, not the process one. That context carries
// the consumer span, so anything the handler does — a Redis write, a fan-out to
// SSE clients — hangs off the publish that caused it instead of off nothing.
func Subscribe(ctx context.Context, brokers []string, topics []string, groupID string,
	lg *zap.Logger, fn func(ctx context.Context, topic string, c SeatChange)) {

	for _, topic := range topics {
		go func(topic string) {
			r := kafka.NewReader(kafka.ReaderConfig{
				Brokers: brokers,
				Topic:   topic,
				GroupID: groupID,
				// Start at the end. A gateway that has just started has no
				// browsers connected yet, so replaying a day of history would be
				// pure work for nobody — and the seat map is refetched on connect
				// anyway, which is the real source of truth for current state.
				StartOffset: kafka.LastOffset,
				MinBytes:    1,
				MaxBytes:    1 << 20,
				MaxWait:     250 * time.Millisecond,
			})
			// Registered for the client-side metrics SigNoz's Kafka view wants,
			// which no Go client can get from JMX. See pkg/events/metrics.go.
			untrack := track(r, topic, groupID, lg)
			defer untrack()
			defer func() { _ = r.Close() }()

			for {
				m, err := r.ReadMessage(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					lg.Warn("kafka read failed", zap.String("topic", topic), zap.Error(err))
					// Do not spin on a broker that is down.
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
					continue
				}
				var c SeatChange
				if err := json.Unmarshal(m.Value, &c); err != nil {
					lg.Warn("undecodable seat change", zap.String("topic", topic), zap.Error(err))
					continue
				}

				// The handler runs INSIDE the consumer span, so whatever it does
				// — a Redis write, a fan-out to SSE clients — hangs off the
				// publish that caused it rather than off nothing.
				func() {
					msgCtx, end := startConsume(ctx, m, groupID)
					defer end()
					fn(msgCtx, topic, c)
				}()
			}
		}(topic)
	}
}

// Brokers turns the configured address into what kafka-go wants.
func Brokers(addr string) []string {
	if addr == "" {
		return nil
	}
	return []string{addr}
}
