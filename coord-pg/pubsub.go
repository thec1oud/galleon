package coordpg

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/thec1oud/galleon/coord"
	"github.com/thec1oud/galleon/errs"
)

// maxPayload is the longest NOTIFY payload PostgreSQL takes.
const maxPayload = 7999

// subscriptionBuffer is how many messages a subscription holds for a slow
// reader before it drops them, as at-most-once allows.
const subscriptionBuffer = 256

type pubsub struct{ b *Backend }

// A message's payload is the topic's length, a colon, the topic, then the
// data in base64. Each topic has a channel of its own, named by its hash;
// the topic in the payload tells two topics with one hash apart.
func encodeMessage(topic string, data []byte) string {
	return strconv.Itoa(len(topic)) + ":" + topic + base64.StdEncoding.EncodeToString(data)
}

func decodeMessage(payload string) (coord.Message, bool) {
	n, rest, ok := strings.Cut(payload, ":")
	if !ok {
		return coord.Message{}, false
	}
	size, err := strconv.Atoi(n)
	if err != nil || size < 0 || size > len(rest) {
		return coord.Message{}, false
	}
	data, err := base64.StdEncoding.DecodeString(rest[size:])
	if err != nil {
		return coord.Message{}, false
	}
	return coord.Message{Topic: rest[:size], Data: data}, true
}

func checkTopic(topic string) error {
	if !utf8.ValidString(topic) || strings.ContainsRune(topic, 0) {
		return errs.New(errs.InvalidArgument, CodeInvalidArgument, "a topic must be valid UTF-8 without NUL bytes")
	}
	return nil
}

// Publish sends data to the nodes subscribed to topic now. It fails with
// CodeMessageTooLarge if the topic and the data, base64-encoded, don't fit
// in a NOTIFY payload.
func (p pubsub) Publish(ctx context.Context, topic string, data []byte) error {
	if err := p.b.ready(); err != nil {
		return err
	}
	if err := checkTopic(topic); err != nil {
		return err
	}
	payload := encodeMessage(topic, data)
	if len(payload) > maxPayload {
		return errs.New(errs.InvalidArgument, CodeMessageTooLarge,
			fmt.Sprintf("a message on topic %q is %d bytes encoded, over the %d a NOTIFY payload takes", topic, len(payload), maxPayload))
	}
	if _, err := p.b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channelName(p.b.cfg.Schema, "p", topic), payload); err != nil {
		return p.b.fail(ctx, err)
	}
	return nil
}

// Subscribe returns once this node listens on the topic's channel, so a
// message published after it returns is delivered, unless it is lost.
func (p pubsub) Subscribe(ctx context.Context, topic string) (coord.Subscription, error) {
	if err := p.b.ready(); err != nil {
		return nil, err
	}
	if err := checkTopic(topic); err != nil {
		return nil, err
	}
	s := &subscription{b: p.b, topic: topic, channel: channelName(p.b.cfg.Schema, "p", topic),
		ch: make(chan coord.Message, subscriptionBuffer), done: make(chan struct{})}
	p.b.mu.Lock()
	if p.b.subs[topic] == nil {
		p.b.subs[topic] = map[*subscription]struct{}{}
	}
	p.b.subs[topic][s] = struct{}{}
	p.b.mu.Unlock()
	if err := p.b.lis.listen(ctx, s.channel); err != nil {
		p.b.mu.Lock()
		delete(p.b.subs[topic], s)
		if len(p.b.subs[topic]) == 0 {
			delete(p.b.subs, topic)
		}
		p.b.mu.Unlock()
		return nil, p.b.fail(ctx, err)
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
		}
		_ = s.Close()
	}()
	return s, nil
}

type subscription struct {
	b       *Backend
	topic   string
	channel string
	ch      chan coord.Message

	once sync.Once
	done chan struct{} // closed by Close
}

func (s *subscription) C() <-chan coord.Message { return s.ch }

func (s *subscription) Close() error {
	s.once.Do(func() {
		s.b.mu.Lock()
		delete(s.b.subs[s.topic], s)
		if len(s.b.subs[s.topic]) == 0 {
			delete(s.b.subs, s.topic)
		}
		close(s.ch) // under b.mu, so deliver never sends on it after
		s.b.mu.Unlock()
		s.b.lis.unlisten(s.channel)
		close(s.done)
	})
	return nil
}

// deliver hands a message to the topic's subscriptions, dropping it for
// any that is full.
func (b *Backend) deliver(m coord.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[m.Topic] {
		select {
		case s.ch <- coord.Message{Topic: m.Topic, Data: append([]byte(nil), m.Data...)}:
		default:
		}
	}
}
