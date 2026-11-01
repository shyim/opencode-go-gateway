package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const jsonNull = "null"

// gateway holds the dependencies and settings for the public HTTP API.
type gateway struct {
	upstream *upstream
	apiKey   string
	timeout  time.Duration
}

// upstream holds the OpenCode endpoints and the cached models.dev catalog.
type upstream struct {
	catalogURL, consoleURL, goURL *url.URL
	key                           string
	clientVersion                 string
	clientName, clientChannel     string
	http                          *http.Client
	mu                            sync.Mutex
	cached                        []model
	expires                       time.Time
}

// requestIdentity separates a request's session from its shared cache routing key.
type requestIdentity struct {
	session, affinity, parent, project string
}

type model struct {
	ID         string
	ProviderID string
	Name       string
}

type catalogProvider struct {
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	ID         string                     `json:"id"`
	Type       string                     `json:"type"`
	Name       string                     `json:"name"`
	Status     string                     `json:"status"`
	Modalities catalogModalities          `json:"modalities"`
	Cost       json.RawMessage            `json:"cost"`
	API        map[string]json.RawMessage `json:"api"`
}

type catalogModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type catalogCost struct {
	Text catalogTextCost `json:"text"`
}

type catalogTextCost struct {
	Input  *catalogPrice `json:"input"`
	Output *catalogPrice `json:"output"`
}

type catalogPrice struct {
	Price     *float64               `json:"price"`
	Cache     json.RawMessage        `json:"cache"`
	Overrides []catalogPriceOverride `json:"overrides"`
}

type catalogPriceOverride struct {
	Price *float64        `json:"price"`
	Cache json.RawMessage `json:"cache"`
}

type flushingWriter struct {
	io.Writer
	flusher http.Flusher
}
