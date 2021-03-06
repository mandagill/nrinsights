// TODO: docs
// TODO: tests
// TODO: pluggable logger

package nrinsights

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jeremywohl/flatten"
)

const (
	// How often event batches are sent.
	sendInterval = 60 * time.Second

	// We queue batches when New Relic is unresponsive.
	// sendInterval * sendQueueSize == <number of seconds before we start dropping event batches>
	sendQueueSize = 20

	// Maximum events per call, defined by New Relic.
	maxEventsPerCall = 1000

	// Maximum size per call, defined by New Relic.
	maxSizePerCall = 5000000

	// Default HTTP timeout.
	defaultHttpTimeout = 10 * time.Second

	// Fast HTTP timeout, for exit cleanup.
	fastHttpTimeout = 2 * time.Second
)

type SeparatorStyle int

const (
	_ SeparatorStyle = 0

	// Separate nested key components with dots, e.g. "a.b.1.c.d"
	DotStyle = SeparatorStyle(flatten.DotStyle)

	// Separate ala Rails, e.g. "a[b][c][1][d]"
	RailsStyle = SeparatorStyle(flatten.RailsStyle)
)

type Connection struct {
	NewRelicAccountId int
	NewRelicAppId     int
	InsightsAPIKey    string

	// HTTP request params to be ignored
	QueryParamsToSkip []string

	// Whether to flatten POST bodies and assign separate keys to each -- these must uniformly be JSON bodies
	FlattenPosts bool

	// POST parameter formatting, defaults to DotStyle
	FlattenStyle SeparatorStyle

	host        string          // cache
	skipParams  map[string]bool // cache
	eventQueue  []string
	queueBytes  int
	events      chan string
	batches     chan string
	eventsDone  chan bool
	batchesDone chan bool
	unsent      *list.List
	httpTimeout time.Duration
}

type Event struct {
	values map[string]interface{}
}

func (e *Event) Set(name string, value interface{}) {
	e.values[name] = value
}

func (c *Connection) Start() {
	// skip param lookup
	c.skipParams = make(map[string]bool)
	for _, p := range c.QueryParamsToSkip {
		c.skipParams[strings.ToLower(p)] = true
	}

	c.events = make(chan string, 10) // buffer a bit to amortize cost of batching under high load
	c.batches = make(chan string, sendQueueSize)
	c.eventsDone = make(chan bool, 1)
	c.batchesDone = make(chan bool, 1)
	c.unsent = list.New()
	c.httpTimeout = defaultHttpTimeout

	if hostname, err := os.Hostname(); err != nil {
		c.host = "<unknown>"
	} else {
		c.host = hostname
	}

	if c.FlattenStyle == 0 {
		c.FlattenStyle = DotStyle
	}

	go c.makeBatches()
	go c.sendBatches()
}

func (c *Connection) StopAndFlush() {
	close(c.events)
	<-c.eventsDone
	close(c.batches)
	<-c.batchesDone
}

func (c *Connection) NewEvent() *Event {
	var e Event
	e.values = make(map[string]interface{})

	// defined by New Relic
	e.Set("accountId", c.NewRelicAccountId)
	if c.NewRelicAppId != 0 {
		e.Set("appId", c.NewRelicAppId)
	}
	e.Set("eventType", "Transaction")
	e.Set("timestamp", time.Now().Unix())

	e.Set("host", c.host)

	return &e
}

// Create an event with values extracted from http.Request.  Sets "url" and "method".
// For each query parameter, sets a "p:<key>" and the first value associated with <key> in the query.
// If c.FlattenPosts is true, POST bodies are considered to be JSON strings and each key-value
// pair sent separately.  (Any hierarchy in this JSON is flattened into a one-dimensional map with compound keys.)
// If c.FlattenPosts is false (default), POST bodies are sent as a single "body" value.
func (c *Connection) MakeEventFromRequest(r *http.Request) (*Event, error) {
	e := c.NewEvent()
	e.Set("url", r.URL.Path)
	e.Set("method", r.Method)

	qvalues := r.URL.Query()
	for key := range qvalues {
		if _, ok := c.skipParams[strings.ToLower(key)]; ok {
			continue
		}
		e.Set("p:"+key, qvalues.Get(key))
	}

	if r.Method == "POST" {
		bodybuf, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %v", err)
		}
		bodyreader := ioutil.NopCloser(bytes.NewBuffer(bodybuf))
		r.Body = bodyreader

		if c.FlattenPosts {
			var nested, flat map[string]interface{}

			err = json.Unmarshal(bodybuf, &nested)
			if err != nil {
				log.Printf("failed to unmarshal request json: %v; storing body as one string", err)
				e.Set("body", string(bodybuf[:]))
				goto done
			}

			flat, err = flatten.Flatten(nested, "p:", flatten.SeparatorStyle(c.FlattenStyle))
			if err != nil {
				log.Printf("failed to flatten request params: %v; storing body as one string", err)
				e.Set("body", string(bodybuf[:]))
				goto done
			}

			for k, v := range flat {
				e.Set(k, v)
			}
		} else {
			e.Set("body", string(bodybuf[:]))
		}

	done:
	}

	return e, nil
}

type Mutator func(r *http.Request, e *Event)

// Sets all the values from MakeEventFromRequest and adds call time "duration" in floating point seconds,
// and resulting "status-code".
func (c *Connection) Middleware(h http.Handler, fn Mutator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event, err := c.MakeEventFromRequest(r)
		if err != nil {
			log.Printf("insights middleware: failed to make event from request: %v", err)
			h.ServeHTTP(w, r)
			return
		}

		if fn != nil {
			fn(r, event)
		}

		start := time.Now()
		captureWriter := &captureStatus{ResponseWriter: w, status: 200}

		h.ServeHTTP(captureWriter, r)

		event.Set("duration", time.Since(start).Seconds())
		event.Set("status-code", captureWriter.status)

		c.RegisterEvent(event)
	})
}

type captureStatus struct {
	http.ResponseWriter
	status int
}

func (cs *captureStatus) WriteHeader(status int) {
	cs.status = status
	cs.ResponseWriter.WriteHeader(status)
}

func (c *Connection) RegisterEvent(e *Event) error {
	asjson, err := json.Marshal(e.values)
	if err != nil {
		return fmt.Errorf("could not marshal event: %v", err)
	}

	c.events <- string(asjson[:])

	return nil
}

func (c *Connection) makeBatches() {
	ticker := time.NewTicker(sendInterval)

outer:
	for {
		select {
		case e, open := <-c.events:
			if !open {
				break outer
			}

			c.eventQueue = append(c.eventQueue, e)
			c.queueBytes += len(e)

			// If we're within 90% of New Relic space limits, batch early.
			if len(c.eventQueue) > maxEventsPerCall*0.90 || c.queueBytes > maxSizePerCall*0.90 {
				c.makeBatch()
			}

		case <-ticker.C:
			c.makeBatch()
		}
	}

	c.makeBatch() // flush remaining
	c.eventsDone <- true
}

func (c *Connection) makeBatch() {
	if len(c.eventQueue) == 0 {
		return
	}

	batch := "[" + strings.Join(c.eventQueue, ",") + "]"

	select {
	case c.batches <- batch:
	default:
	}

	c.eventQueue = nil
	c.queueBytes = 0
}

func (c *Connection) sendBatches() {
	for batch := range c.batches {
		c.unsent.PushBack(batch)
		c.sendUnsent()
	}

	c.httpTimeout = fastHttpTimeout // decrease for prompt exit
	c.sendUnsent()

	c.batchesDone <- true
}

func (c *Connection) sendUnsent() {
	var next *list.Element
	for elem := c.unsent.Front(); elem != nil; elem = next {
		next = elem.Next()

		if c.sendBatch(elem.Value.(string)) {
			c.unsent.Remove(elem)
		}
	}
}

func (c *Connection) sendBatch(batch string) bool {
	url := fmt.Sprintf("https://insights-collector.newrelic.com/v1/accounts/%d/events", c.NewRelicAccountId)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(batch)))
	if err != nil {
		log.Printf("insights sendBatch: failed to create http request: %v; queueing for resend", err)
		return false
	}
	req.Header.Set("X-Insert-Key", c.InsightsAPIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: c.httpTimeout,
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("insights sendBatch: failed to send http request: %v; queueing for resend", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Printf("insights sendBatch: failed to read response body: %v; queueing for resend")
			return false
		}

		log.Printf("insights sendBatch: non-200 result: %d [%s]; queueing for resend", resp.StatusCode, body)
	}

	return true
}
