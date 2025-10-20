package ddslog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Handler implements Handler by wrapping a DataDog API client.
type DDHandler struct {
	// ---- stolen from log/slog.commonHandler ----
	opts              slog.HandlerOptions
	preformattedAttrs []byte
	// groupPrefix is for the text handler only.
	// It holds the prefix for groups that were already pre-formatted.
	// A group will appear here when a call to WithGroup is followed by
	// a call to WithAttrs.
	groupPrefix string
	groups      []string // all groups started from WithGroup
	nOpenGroups int      // the number of groups opened in preformattedAttrs

	// --- the rest is specific to this handler ---
	// JSON is formatted per-thread by .Handle() then sent on for batching...
	formatted chan json.RawMessage // channel to send the JSON-encoded records to *commonHandler
	// then the batches are sent to DataDog via GZIP-compressed HTTP requests.
	batched chan []json.RawMessage // batches of JSON-encoded records, < 1MiB each and < 5MiB total.

	mux *sync.Mutex // protects w.
	w   io.Writer   // if non-nil, also write to this writer.
}

func (c Config) WithDefaults() Config {
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	if c.Service == "" {
		c.Service = "ddslog"
	}
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	if c.Service == "" {
		c.Service = filepath.Base(os.Args[0])
	}

	if c.Batch.MaxBatchSizeBytes == 0 {
		c.Batch.MaxBatchSizeBytes = 5 * 1024 * 1024
	}
	if c.Batch.MaxLogSizeBytes == 0 {
		c.Batch.MaxLogSizeBytes = 1024 * 1024
	}
	if c.Batch.MaxLogSizeEntries == 0 {
		c.Batch.MaxLogSizeEntries = 1000 // 1000 messages per batch
	}
	if c.Batch.ForceSendAfter == 0 {
		c.Batch.ForceSendAfter = 3 * time.Second // force send after 3
	}

	if c.Opts.ReplaceAttr == nil {
		c.Opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "msg" {
				a.Key = "message"
			}
			return a
		}
	} else {
		oldrepl := c.Opts.ReplaceAttr
		c.Opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "msg" {
				a.Key = "message"
			}
			return oldrepl(groups, a)
		}
	}
	return c
}

// Find the DataDog target to send logs to based on the API key by sending similar requests to all known targets.
func FindTarget(ctx context.Context, apiKey string) (Target, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dst := make(chan Target, 1)
	for _, v := range Targets() {
		go func(target Target) {
			if err := ValidateAPIKey(ctx, apiKey, target); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("ddslog: cannot validate API key %s for target %s: %v", apiKey, target, err)
				}
				return
			}

			dst <- target
		}(v)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case target := <-dst:
		return target, nil
	}
}

// Validate the API key against a specified DataDog target.
func ValidateAPIKey(ctx context.Context, apiKey string, target Target) error {
	if apiKey == "" {
		return fmt.Errorf("ddslog: cannot validate empty API key")
	}
	if target == "" {
		return fmt.Errorf("ddslog: cannot validate API key with empty target host")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://%s/api/v1/validate", target)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ddslog: cannot create request to validate API key: %w", err)
	}
	r.Header["DD-API-KEY"] = []string{apiKey}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return fmt.Errorf("ddslog: cannot validate API key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ddslog: unexpected status code %d when validating API key: %s", resp.StatusCode, body)
	}
	return nil
}

func sendBatches(ctx context.Context, c *http.Client, target Target, apiKey string, batches <-chan []json.RawMessage) error {
	buf := bytes.NewBuffer(make([]byte, 0, 5*MiB))
	zw := gzip.NewWriter(buf)

	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	url := fmt.Sprintf("https://http-intake.logs.%s/api/v2/logs", target)
	basereq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("ddslog: cannot create request: %w", err)
	}

	basereq.Header.Set("Content-Type", "application/json")
	basereq.Header["DD-API-KEY"] = []string{apiKey}
	basereq.Header.Set("Content-Encoding", "gzip")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch := <-batches:
			if len(batch) == 0 {
				log.Printf("empty batch received, skipping")
				continue // no data to send
			}
			if len(batch) > 1000 {
				panic(fmt.Sprintf("ddslog: batch size %d exceeds datadog hard limit of 1000 messages!", len(batch)))
			}
			// set up the request, cloning it so we can reuse the same request
			r := basereq.Clone(ctx)
			buf.Reset()
			zw.Reset(buf)
			if err := enc.Encode(batch); err != nil {
				log.Printf("ddslog: cannot encode batch: %v", err)
				continue
			}
			if err := zw.Close(); err != nil {
				log.Printf("ddslog: cannot close gzip writer: %v", err)
				continue
			}

			// copy the body so we can send it in a goroutine and still collect new batches
			r.Body = io.NopCloser(bytes.NewReader(bytes.Clone(buf.Bytes())))
			r.ContentLength = int64(buf.Len())
			go func() {
				resp, err := c.Do(r)
				if err != nil {
					log.Printf("ddslog: cannot send batch: %v", err)
				}
				if resp.StatusCode > 299 || resp.StatusCode < 200 {
					body, _ := io.ReadAll(resp.Body)
					if resp.Body != nil {
						resp.Body.Close() // we don't care about the body
					}
					log.Printf("ddslog: unexpected status code %d: %s", resp.StatusCode, body)
				}
			}()

		}
	}
}

const MiB = 1024 * 1024

// take formatted messages and batch them into appropriate chunks to send to DataDog.
// we send a batch of messages when:
// - the batch is full (1000 messages or 5 MiB)
// - a non-full batch has sat idle for 3 seconds
func batchHandled(ctx context.Context, cfg BatchConfig, dst chan<- []json.RawMessage, src <-chan json.RawMessage) {
	prev, batch := make([]json.RawMessage, 0, 1000), make([]json.RawMessage, 0, 1000)

	var batchsize int
	tick := time.NewTimer(3 * time.Second)
	go tick.Stop()
	send := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case dst <- batch:
			prev, batch = batch[:0], prev[:0]
			batchsize = 0
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			send()
		case m := <-src:
			if len(m) > cfg.MaxBatchSizeBytes {
				log.Printf("ddslog: skipping oversized message of %d bytes", len(m))
				continue
			}
			if len(batch) >= cfg.MaxLogSizeEntries || len(m)+batchsize > cfg.MaxBatchSizeBytes {
				send()
			}
			batch = append(batch, m)
			batchsize += len(m) + 3 // '{},'
			if cfg.ForceSendAfter > 0 {
				tick.Reset(cfg.ForceSendAfter)
			}
			// we want to flush the batch when there's a non-empty log batch been no activity for 3 seconds
		}
	}
}

func (h *DDHandler) WithAttrs(as []slog.Attr) slog.Handler              { return h.withAttrs(as) }
func (h *DDHandler) WithGroup(name string) slog.Handler                 { return h.withGroup(name) }
func (h *DDHandler) Enabled(ctx context.Context, level slog.Level) bool { return h.enabled(level) }
func (h *DDHandler) Handle(ctx context.Context, r slog.Record) error    { return h.handle(ctx, r) }

var _ slog.Handler = &DDHandler{}

type BatchConfig struct {
	MaxBatchSizeBytes int           // optional, if 0, the default is 5 MiB
	MaxLogSizeBytes   int           // optional, if 0, the default is 1 MiB.
	MaxLogSizeEntries int           // optional, if 0, the default is 1000.
	ForceSendAfter    time.Duration // optional, if 0, the default is 3 seconds; if < 0, no forced send.
}
type Config struct {
	*http.Client // optional, if nil, a default client is used.

	Batch BatchConfig // Optional.

	Opts     slog.HandlerOptions // optional.
	APIKey   string              // required, DataDog API key.
	Writer   io.Writer           // optional, if non-nil, also write to this writer.
	Tags     string              `json:"ddtags,omitempty"`   // tags to be sent with each log.
	Source   string              `json:"ddsource,omitempty"` // source to be sent with each log.
	Hostname string              `json:"hostname,omitempty"` // hostname to be sent with each log. Hostname is YOUR machine's hostname, not DataDog's.
	Service  string              `json:"service,omitempty"`  // service to be sent with each log.
	Target   Target              `json:"url,omitempty"`      // Which DataDog site to send logs to. Optional: if empty, New() attempts to find the target by sending requests to all known targets.
}

/*
Hostname of the DataDog site to send logs to.
Site	Site URL	Site Parameter	Location
US1	https://app.datadoghq.com	datadoghq.com	US
US3	https://us3.datadoghq.com	us3.datadoghq.com	US
US5	https://us5.datadoghq.com	us5.datadoghq.com	US
EU1	https://app.datadoghq.eu	datadoghq.eu	EU (Germany)
US1-FED	https://app.ddog-gov.com	ddog-gov.com	US
AP1	https://ap1.datadoghq.com	ap1.datadoghq.com	Japan
AP2	https://ap2.datadoghq.com	ap2.datadoghq.com	Australia
*/
type Target string

/*
Site	Site URL	Site Parameter	Location
US1	https://app.datadoghq.com	datadoghq.com	US
US3	https://us3.datadoghq.com	us3.datadoghq.com	US <--- DEFAULT
US5	https://us5.datadoghq.com	us5.datadoghq.com	US
EU1	https://app.datadoghq.eu	datadoghq.eu	EU (Germany)
US1-FED	https://app.ddog-gov.com	ddog-gov.com	US
AP1	https://ap1.datadoghq.com	ap1.datadoghq.com	Japan
AP2	https://ap2.datadoghq.com	ap2.datadoghq.com	Australia
*/
const (
	TargetDefault Target = "us3.datadoghq.com"
	TargetUS1     Target = "app.datadoghq.com"
	TargetUS3     Target = "us3.datadoghq.com"
	TargetUS5     Target = "us5.datadoghq.com"
	TargetEU1     Target = "datadoghq.eu"
	TargetUS1FED  Target = "app.ddog-gov.com"
	TargetAP1     Target = "ap1.datadoghq.com"
	TargetAP2     Target = "ap2.datadoghq.com"
)

func Targets() []Target {
	return []Target{
		TargetUS1,
		TargetUS3,
		TargetUS5,
		TargetEU1,
		TargetUS1FED,
		TargetAP1,
		TargetAP2,
	}
}

// New creates a new DataDog log handler with the specified configuration.
func New(ctx context.Context, cfg Config) (h *DDHandler, err error) {
	cfg = cfg.WithDefaults()
	if cfg.Batch.MaxLogSizeBytes > MiB {
		return nil, fmt.Errorf("ddslog: MaxLogSizeBytes %d is too large, must be <= 1 MiB", cfg.Batch.MaxLogSizeBytes)
	}
	if cfg.Batch.MaxLogSizeEntries > 1000 {
		return nil, fmt.Errorf("ddslog: MaxLogSizeEntries %d is too large: must be <= 1000", cfg.Batch.MaxLogSizeEntries)
	}
	if cfg.Batch.MaxBatchSizeBytes > 5*MiB {
		return nil, fmt.Errorf("ddslog: MaxBatchSizeBytes %d is too large: must be <= 5 MiB", cfg.Batch.MaxBatchSizeBytes)
	}
	if cfg.Target == "" {
		log.Printf("ddslog: no target specified... finding target for api key starting with %s", cfg.APIKey[:6])

		cfg.Target, err = FindTarget(ctx, cfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("ddslog: cannot find target for API key starting with %s: %w", cfg.APIKey[:6], err)
		}
		log.Printf("ddslog: found target %s for API key starting with %s", cfg.Target, cfg.APIKey[:6])

	} else if err := ValidateAPIKey(ctx, cfg.APIKey, cfg.Target); err != nil {
		return nil, fmt.Errorf("ddslog: cannot validate API key %s for target %s: %w", cfg.APIKey, cfg.Target, err)
	}

	h = (&DDHandler{
		opts:              cfg.Opts,
		preformattedAttrs: make([]byte, 0, 1024),
		groupPrefix:       "",
		groups:            make([]string, 0, 10),
		nOpenGroups:       0,
		formatted:         make(chan json.RawMessage, 1000),
		batched:           make(chan []json.RawMessage, 1),
		mux:               &sync.Mutex{}, // protect h.w
		w:                 cfg.Writer,
	}).withAttrs([]slog.Attr{
		slog.String("ddsource", cfg.Source),
		slog.String("ddtags", cfg.Tags),
		slog.String("hostname", cfg.Hostname),
		slog.String("service", cfg.Service),
	})

	go batchHandled(ctx, cfg.Batch, h.batched, h.formatted)
	// start two goroutines to send batches of logs in case of high load
	go sendBatches(ctx, &http.Client{}, cfg.Target, cfg.APIKey, h.batched)
	go sendBatches(ctx, cfg.Client, cfg.Target, cfg.APIKey, h.batched)
	return h, nil
}
