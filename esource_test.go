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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylelemons/godebug/diff"
)

type syncResponseWriter struct {
	*httptest.ResponseRecorder
	flush chan bool
}

func (w *syncResponseWriter) Flush() { w.flush <- true }

func record(buffer int) *syncResponseWriter {
	return &syncResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flush:            make(chan bool, buffer),
	}
}

func TestBasic(t *testing.T) {
	tests := []struct {
		desc    string
		headers http.Header
		events  []Event
		output  string
	}{
		{
			desc: "data types",
			events: []Event{
				{
					Type: "foo",
					Data: "bar\nbaz\n",
				},
				{
					Type: "spam",
					Data: map[string][]string{
						"eggs": {"over", "easy"},
					},
				},
				{
					Type: "hello",
				},
			},
			output: `id: 0
event: foo
data: bar
data: baz
data: 

id: 1
event: spam
data: {"eggs":["over","easy"]}

id: 2
event: hello

`,
		},
		{
			desc: "last id",
			headers: http.Header{
				"Last-Event-Id": {"1"},
			},
			events: []Event{
				{Type: "a"},
				{Type: "b"},
				{Type: "c"},
			},
			output: `id: 2
event: c

`,
		},
	}

	for _, test := range tests {
		rw := record(len(test.events))
		req := &http.Request{
			RemoteAddr: "test",
			Header:     test.headers,
		}

		es := New()
		for _, ev := range test.events {
			es.Events <- ev
		}

		done := make(chan bool)
		go func() {
			defer close(done)
			es.ServeHTTP(rw, req)
		}()

		time.Sleep(1 * time.Millisecond)
		es.Close()
		<-done

		if got, want := rw.Code, 200; got != want {
			t.Errorf("%s: code = %v, want %v", test.desc, got, want)
		}
		if got, want := rw.HeaderMap.Get("Content-Type"), ContentType; got != want {
			t.Errorf("%s: ctype = %q, want %q", test.desc, got, want)
		}
		if got, want := rw.Body.String(), test.output; got != want {
			t.Errorf("%s: body mismatch:\n%s", test.desc, diff.Diff(got, want))
		}
	}
}

func TestFlush(t *testing.T) {
	tests := []struct {
		input  Event
		output string
	}{
		{
			input: Event{
				Type: "first",
				Data: "value",
			},
			output: `id: 0
event: first
data: value

`,
		},
		{
			input: Event{
				Type: "second",
				Data: "value",
			},
			output: `id: 1
event: second
data: value

`,
		},
	}

	rw := record(0)
	req := &http.Request{
		RemoteAddr: "test",
	}
	es := New()

	done := make(chan bool)
	go func() {
		defer close(done)
		es.ServeHTTP(rw, req)
	}()

	for i, test := range tests {
		rw.Body.Reset()
		es.Events <- test.input
		<-rw.flush
		if got, want := rw.Body.String(), test.output; got != want {
			t.Errorf("%d. body mismatch:\n%s", i, diff.Diff(got, want))
		}
	}

	es.Close()
	<-done
}
