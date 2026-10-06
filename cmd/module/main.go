package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/dio/kona/internal/source"
)

type config struct {
	Source   source.Config `json:"source"`
	PollMS   int           `json:"poll_ms"`
	MaxAgeMS int           `json:"max_age_ms"`
}
type configFactory struct {
	shared.EmptyHttpFilterConfigFactory
}
type factory struct {
	shared.EmptyHttpFilterFactory
	cfg    config
	store  source.Store
	handle shared.HttpFilterConfigHandle
	stop   chan struct{}
	wg     sync.WaitGroup
	// These fields are accessed only on Envoy's main dispatcher.
	destroyed bool
	pending   bool
}

func init() {
	sdk.RegisterHttpFilterConfigFactories(map[string]shared.HttpFilterConfigFactory{"kona": &configFactory{}})
}
func (*configFactory) Create(h shared.HttpFilterConfigHandle, b []byte) (shared.HttpFilterFactory, error) {
	c := config{PollMS: 500, MaxAgeMS: 3000}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if err := c.Source.Validate(); err != nil {
		return nil, err
	}
	if c.PollMS < 50 || c.MaxAgeMS < c.PollMS {
		return nil, fmt.Errorf("require poll_ms >= 50 and max_age_ms >= poll_ms")
	}
	f := &factory{cfg: c, handle: h, stop: make(chan struct{})}
	if len(c.Source.Inline) > 0 {
		if err := f.store.Publish(c.Source.Inline, time.Now()); err != nil {
			return nil, err
		}
		return f, nil
	}
	scheduler := h.GetScheduler()
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-timer.C:
				scheduler.Schedule(f.refresh)
				timer.Reset(time.Duration(c.PollMS) * time.Millisecond)
			}
		}
	}()
	return f, nil
}
func (f *factory) refresh() {
	if f.destroyed || f.pending {
		return
	}
	if f.cfg.Source.Filename != "" {
		if b, err := source.ReadFile(f.cfg.Source.Filename); err == nil {
			_ = f.store.Publish(b, time.Now())
		}
		return
	}
	r := f.cfg.Source.Remote
	f.pending = true
	result, _ := f.handle.HttpCallout(r.Cluster, [][2]string{{":method", "GET"}, {":scheme", "https"}, {":authority", r.Authority}, {":path", r.Path}}, nil, 1000, f)
	if result != shared.HttpCalloutInitSuccess {
		f.pending = false
	}
}
func (f *factory) OnHttpCalloutDone(_ uint64, result shared.HttpCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
	f.pending = false
	if f.destroyed || result != shared.HttpCalloutSuccess {
		return
	}
	ok := false
	for _, h := range headers {
		if h[0].ToUnsafeString() == ":status" && h[1].ToUnsafeString() == "200" {
			ok = true
		}
	}
	if !ok {
		return
	}
	var b []byte
	for _, part := range body {
		if part.Len > uint64(source.MaxBytes-len(b)) {
			return
		}
		b = append(b, part.ToUnsafeBytes()...)
	}
	_ = f.store.Publish(b, time.Now())
}
func (f *factory) OnDestroy() { f.destroyed = true; close(f.stop); f.wg.Wait() }
func (f *factory) Create(h shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{factory: f, handle: h}
}

type filter struct {
	shared.EmptyHttpFilter
	factory *factory
	handle  shared.HttpFilterHandle
}

func (f *filter) OnRequestHeaders(_ shared.HeaderMap, _ bool) shared.HeadersStatus {
	k, ok := f.handle.GetMetadataString(shared.MetadataSourceTypeRoute, "kona", "key")
	age := time.Duration(f.factory.cfg.MaxAgeMS) * time.Millisecond
	if len(f.factory.cfg.Source.Inline) > 0 {
		age = 0
	}
	if ok {
		if b, found := f.factory.store.Lookup(k.ToUnsafeString(), time.Now(), age); found {
			f.handle.SendLocalResponse(200, [][2]string{{"content-type", "application/json"}}, b, "kona_data")
			return shared.HeadersStatusStop
		}
	}
	f.handle.SendLocalResponse(503, nil, []byte("data unavailable\n"), "kona_unavailable")
	return shared.HeadersStatusStop
}
func main() {}
