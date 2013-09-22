// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package esource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

var (
	// ContentType is sent as the content type for event source handlers
	ContentType = "text/event-stream;charset=utf-8"

	// BufferSize is the number of events to buffer per connection before dropping
	BufferSize = 256

	// DefaultBacklog is the default number of events to keep in memory
	DefaultBacklog = 256
)

// An Event represents data that can be sent to an EventSource
type Event struct {
	ID   int         // set by the esource library
	Type string      // event type (may not contain newlines or colons)
	Data interface{} // string (sent raw) or object (will be json encoded) or nil (nothing)
}

// WriteTo writes the wire encoding of the event to the given wire.
func (e *Event) WriteTo(w io.Writer) (int64, error) {
	// TODO(kevlar): buffer pool? cached encoding?
	buf := new(bytes.Buffer)
	fmt.Fprintln(buf, "id:", e.ID)
	fmt.Fprintln(buf, "event:", e.Type)

	switch d := e.Data.(type) {
	case string:
		for _, line := range strings.Split(d, "\n") {
			fmt.Fprintln(buf, "data:", line)
		}
	case nil:
	default:
		fmt.Fprintf(buf, "data: ")
		if err := json.NewEncoder(buf).Encode(d); err != nil {
			return 0, err
		}
	}
	fmt.Fprintln(buf)
	return buf.WriteTo(w)
}

// An EventSource represents a stream of events that can handle multiple clients.
//
// All methods on EventSource are goroutine-safe, as is sending messages on the
// Events channel.  Methods may not be called and Events may not be sent after
// the Close method has been called.
type EventSource struct {
	// Events receives events to be sent to all listening connections.
	Events chan<- Event

	// control receives control commands.
	control chan<- interface{}
}

// New creates a new EventSource.
func New() *EventSource {
	input := make(chan Event)
	control := make(chan interface{})

	es := &EventSource{
		Events:  input,
		control: control,
	}

	go es.run(input, control)
	return es
}

// run is the inner loop of the EventSource and should be run in its
// own goroutine.
func (es *EventSource) run(input chan Event, control chan interface{}) {
	var (
		nextID  = 0
		offset  = 0
		backlog = DefaultBacklog
		events  = make([]Event, 0, BufferSize)
		clients = make(map[chan Event]bool)
	)

	defer func() {
		for ch := range clients {
			close(ch)
		}
	}()

	ensure := func(avail int) {
		if len(events) > backlog-avail {
			remove := len(events) - backlog + avail
			events, offset = events[remove:], offset+remove
		}
	}

	push := func(ch chan Event, ev Event) {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(clients, ch)
		}
	}

	bound := func(v, lo, hi int) int {
		if v < lo {
			return lo
		} else if v >= hi {
			return hi
		}
		return v
	}

	for {
		select {
		case ev, ok := <-input:
			if !ok {
				return
			}

			ev.ID, nextID = nextID, nextID+1

			ensure(1)
			events = append(events, ev)

			for ch := range clients {
				push(ch, ev)
			}
		case msg := <-control:
			switch msg := msg.(type) {
			case setBacklog:
				backlog = msg.backlog
				ensure(0)
			case startStream:
				msg.old <- events[bound(msg.startID-offset, 0, len(events)):]
				clients[msg.events] = true
			}
		}
	}
}

// Close causes the EventSource to shut down all connections.  The final
// messages will be flushed to clients asynchronously.
func (es *EventSource) Close() error {
	close(es.Events)
	return nil
}

// setBacklog changes the backlog of the EventSource.
type setBacklog struct {
	backlog int
}

// SetBacklog adjusts the number of messages that will be maintained in memory
// in order to be able to catch up clients which reconnect.  If more events
// have been stored than the given backlog, the excess events will be discarded
// immediately.
func (es *EventSource) SetBacklog(backlog int) {
	es.control <- setBacklog{backlog: backlog}
}

// startStream starts streaming events
type startStream struct {
	startID int
	old     chan []Event
	events  chan Event
}

// Tee delivers the events as they would go to a client.
func (es *EventSource) Tee(startID int) ([]Event, chan Event) {
	start := startStream{
		startID: startID,
		old:     make(chan []Event, 1),
		events:  make(chan Event, BufferSize),
	}

	es.control <- start
	return <-start.old, start.events
}

// ServeHTTP serves the client with events as they come in, and with any
// stored events
func (es *EventSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var startID int
	if start := r.Header.Get("Last-Event-ID"); start != "" {
		if _, err := fmt.Sscanf(start, "%d", &startID); err != nil {
			http.Error(w, "bad Last-Event-ID", http.StatusBadRequest)
			return
		}
		startID++
	}

	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(200)

	flusher, ok := w.(http.Flusher)
	if !ok {
		panic("esource: ServeHTTP called without a Flusher")
	}

	log.Printf("esource: [%s] starting stream", r.RemoteAddr)

	old, events := es.Tee(startID)
	for _, event := range old {
		if _, err := event.WriteTo(w); err != nil {
			log.Printf("esource: [%s] failed to write backlogged event %+v", r.RemoteAddr, event)
			return
		}
	}
	flusher.Flush()

	for event := range events {
		if _, err := event.WriteTo(w); err != nil {
			log.Printf("esource: [%s] failed to write event %+v", r.RemoteAddr, event)
			fmt.Fprintln(w, "\nretry: 0\n")
			return
		}
		flusher.Flush()
	}

	log.Printf("esource: [%s] complete", r.RemoteAddr)
}
