package controlapi

import (
	"sync"
	"sync/atomic"
)

type subscription struct {
	events  chan LifecycleEvent
	dropped atomic.Bool
}

type broker struct {
	mu       sync.Mutex
	capacity int
	nextID   uint64
	topics   map[string]map[uint64]*subscription
}

func newBroker(capacity int) *broker {
	return &broker{
		capacity: capacity,
		topics:   make(map[string]map[uint64]*subscription),
	}
}

func (value *broker) subscribe(topic string) (*subscription, func()) {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.nextID++
	id := value.nextID
	sub := &subscription{events: make(chan LifecycleEvent, value.capacity)}
	if value.topics[topic] == nil {
		value.topics[topic] = make(map[uint64]*subscription)
	}
	value.topics[topic][id] = sub
	return sub, func() {
		value.mu.Lock()
		defer value.mu.Unlock()
		if current, exists := value.topics[topic][id]; exists {
			delete(value.topics[topic], id)
			close(current.events)
		}
	}
}

func (value *broker) publish(topic string, event LifecycleEvent) {
	value.mu.Lock()
	defer value.mu.Unlock()
	for id, sub := range value.topics[topic] {
		select {
		case sub.events <- event:
		default:
			sub.dropped.Store(true)
			delete(value.topics[topic], id)
			close(sub.events)
		}
	}
}
