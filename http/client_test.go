// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package http

import (
	"context"
	"fmt"
	"github.com/spacemonkeygo/monkit/v3/present"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
)

type caller func(ctx context.Context, request *http.Request) (*http.Response, error)

func TestPropagation(t *testing.T) {
	mon := monkit.Package()

	addr, closeServer := startHTTPServer(t)

	defer closeServer()

	ctx := context.Background()
	trace := monkit.NewTrace(monkit.NewId())
	trace.Set(present.SampledKey, true)

	defer mon.Func().RemoteTrace(&ctx, 0, trace)(nil)

	body, header := clientCallWithRetry(t, ctx, addr, func(ctx context.Context, request *http.Request) (*http.Response, error) {
		return TraceRequest(ctx, monkit.ScopeNamed("client"), http.DefaultClient, request)
	})

	s := monkit.SpanFromCtx(ctx)

	expected := fmt.Sprintf("%d/hello/true", s.Id())

	if string(body) != expected {
		t.Fatalf("%s!=%s", string(body), expected)
	}
	if header != "" {
		t.Fatalf("tracestate should be empty: %s", header)
	}
}

// TestForcedSample checks if sampling can be turned on without having trace/span on client side.
func TestForcedSample(t *testing.T) {

	addr, closeServer := startHTTPServer(t)

	defer closeServer()

	body, header := clientCallWithRetry(t, context.Background(), addr, func(ctx context.Context, request *http.Request) (*http.Response, error) {
		request.Header.Set(traceStateHeader, "sampled=true")
		return http.DefaultClient.Do(request)
	})

	expected := "0/hello/true"

	if string(body) != expected {
		t.Fatalf("%s!=%s", string(body), expected)
	}
	if header == "" {
		t.Fatalf("tracestate should not be empty: %s", header)
	}
}

func clientCallWithRetry(t *testing.T, ctx context.Context, addr string, caller caller) (string, string) {
	var err error
	for i := 0; i < 100; i++ {
		body, header, err := clientCall(ctx, addr, caller)
		if err == nil {
			return body, header
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(err)
	return "", ""
}

func clientCall(ctx context.Context, addr string, caller caller) (string, string, error) {
	request, err := http.NewRequest("GET", addr, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := caller(ctx, request)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return string(body), resp.Header.Get(traceStateHeader), nil
}

func startHTTPServer(t *testing.T) (addr string, def func()) {
	mux := http.NewServeMux()
	mon := monkit.Package()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		defer mon.Task()(&ctx)(nil)
		s := monkit.SpanFromCtx(ctx)

		grandParent := int64(0)
		parent, found := s.ParentId()
		if found {
			// we are interested about the parent of the parent,
			// created by the TraceHandler
			monkit.RootSpans(func(s *monkit.Span) {
				if s.Id() == parent {
					grandParent, _ = s.ParentId()
				}
			})
		}
		_, _ = fmt.Fprintf(w, "%d/%s/%v", grandParent, "hello", s.Trace().Get(present.SampledKey))
	})

	listener, err := net.Listen("tcp", ":0")
	addr = fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal("Couldn't start tcp listener", err)
	}

	server := &http.Server{Addr: "localhost:5050", Handler: TraceHandler(mux, monkit.ScopeNamed("server"))}

	go func() {
		_ = server.Serve(listener)
	}()
	return addr, func() {
		_ = server.Close()
		_ = listener.Close()
	}
}
