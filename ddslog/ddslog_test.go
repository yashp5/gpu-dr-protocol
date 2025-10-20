package ddslog_test

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/grene-labs/grene/ddslog"
)

var testAPIKey string

func TestMain(m *testing.M) {
	flag.StringVar(&testAPIKey, "DD_API_KEY", "", "DataDog API key for testing")
	testing.Init()
	flag.Parse()
	if apikey := os.Getenv("DD_API_KEY"); apikey != "" {
		testAPIKey = apikey
	}

	os.Exit(m.Run())
}

var once sync.Once

func TestValidateAPIKey(t *testing.T) {
	skip(t)
	if err := ddslog.ValidateAPIKey(context.Background(), testAPIKey, ddslog.TargetUS3); err != nil {
		t.Fatalf("ddslog.ValidateAPIKey failed: %v", err)
	}
}

func TestFindTargets(t *testing.T) {
	skip(t)
	if target, err := ddslog.FindTarget(t.Context(), testAPIKey); err != nil {
		t.Fatalf("ddslog.FindTarget failed: %v", err)
	} else if target != ddslog.TargetUS3 {
		t.Errorf("expected target %s, got %s", ddslog.TargetUS3, target)
	}
}

func skip(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skipf("-SKIP %s: -short: touches the network", t.Name())
	}
	if testAPIKey == "" {
		once.Do(
			func() {
				t.Logf("HELP: set DD_API_KEY environment variable or -DD_API_KEY flag to run these tests")
			},
		)
		t.Skipf("-SKIP %s: no DD_API_KEY set", t.Name())
	}
}

func TestDDSlog(t *testing.T) {
	skip(t)

	var logbuf bytes.Buffer

	handler, err := ddslog.New(
		t.Context(), ddslog.Config{
			APIKey: testAPIKey,
			Source: "ddslog_test",
			Client: http.DefaultClient,
			Batch: ddslog.BatchConfig{
				ForceSendAfter: 100 * time.Millisecond,
			},
			Writer: &logbuf,
		},
	)
	if err != nil {
		t.Fatalf("ddslog.New failed: %v", err)
	}
	if handler == nil {
		t.Fatalf("ddslog.New returned nil handler")
	}

	log := slog.New(handler)
	log.Info("hello world")
	log.Info("ddslog test started")
	log.Warn("some fake warning", "with attribute", "value")

	time.Sleep((300 * time.Millisecond)) // wait for the batch to be sent
	if !bytes.Contains(logbuf.Bytes(), []byte("ddslog test started")) {
		t.Errorf("expected log to contain 'ddslog test started', got %q", logbuf.String())
	}
}

type LogResponse struct {
	Data  []Data `json:"data,omitempty"`
	Links Links  `json:"links,omitempty"`
	Meta  Meta   `json:"meta,omitempty"`
}
type AttributesAttributes struct {
	CustomAttribute int `json:"customAttribute,omitempty"`
	Duration        int `json:"duration,omitempty"`
}
type Attributes struct {
	AttributesAttributes AttributesAttributes `json:"attributes,omitempty"`
	Host                 string               `json:"host,omitempty"`
	Message              string               `json:"message,omitempty"`
	Service              string               `json:"service,omitempty"`
	Status               string               `json:"status,omitempty"`
	Tags                 []string             `json:"tags,omitempty"`
	Timestamp            time.Time            `json:"timestamp,omitempty"`
}
type Data struct {
	Attributes Attributes `json:"attributes,omitempty"`
	ID         string     `json:"id,omitempty"`
	Type       string     `json:"type,omitempty"`
}
type Links struct {
	Next string `json:"next,omitempty"`
}
type Page struct {
	After string `json:"after,omitempty"`
}
type Warnings struct {
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	Title  string `json:"title,omitempty"`
}
type Meta struct {
	Elapsed   int        `json:"elapsed,omitempty"`
	Page      Page       `json:"page,omitempty"`
	RequestID string     `json:"request_id,omitempty"`
	Status    string     `json:"status,omitempty"`
	Warnings  []Warnings `json:"warnings,omitempty"`
}
