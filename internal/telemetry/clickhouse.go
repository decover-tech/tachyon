package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// RowSink stores finished job records.
type RowSink interface {
	WriteRows(ctx context.Context, rows []JobRow) error
}

// ClickHouse writes job rows over ClickHouse's HTTP interface as
// JSONEachRow, so the control plane needs no native driver.
type ClickHouse struct {
	URL      string // e.g. http://clickhouse:8123
	Table    string // default tachyon.job_records
	User     string
	Password string
	Client   *http.Client
}

func (c *ClickHouse) WriteRows(ctx context.Context, rows []JobRow) error {
	if len(rows) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	table := c.Table
	if table == "" {
		table = "tachyon.job_records"
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("clickhouse url: %w", err)
	}
	q := u.Query()
	q.Set("query", "INSERT INTO "+table+" FORMAT JSONEachRow")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if c.User != "" {
		req.Header.Set("X-ClickHouse-User", c.User)
		req.Header.Set("X-ClickHouse-Key", c.Password)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse insert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("clickhouse insert: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// Batcher buffers rows and writes them to a sink in batches. A sink outage
// never blocks the control plane: rows are retried on the next flush and,
// past MaxBuffered, the oldest are dropped and counted.
type Batcher struct {
	Sink        RowSink
	BatchSize   int           // default 500
	Interval    time.Duration // default 5s
	MaxBuffered int           // default 50,000
	Log         *slog.Logger

	mu      sync.Mutex
	buf     []JobRow
	dropped int
	kick    chan struct{}
}

func (b *Batcher) batchSize() int {
	if b.BatchSize > 0 {
		return b.BatchSize
	}
	return 500
}

func (b *Batcher) maxBuffered() int {
	if b.MaxBuffered > 0 {
		return b.MaxBuffered
	}
	return 50_000
}

func (b *Batcher) kickCh() chan struct{} {
	if b.kick == nil {
		b.kick = make(chan struct{}, 1)
	}
	return b.kick
}

// Add queues a row. Safe for concurrent use; never blocks on the sink.
func (b *Batcher) Add(r JobRow) {
	b.mu.Lock()
	b.buf = append(b.buf, r)
	if over := len(b.buf) - b.maxBuffered(); over > 0 {
		b.buf = append(b.buf[:0:0], b.buf[over:]...)
		b.dropped += over
	}
	full := len(b.buf) >= b.batchSize()
	kick := b.kickCh()
	b.mu.Unlock()
	if full {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

// Dropped is the number of rows discarded because the sink fell behind.
func (b *Batcher) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Pending is the number of rows waiting to be written.
func (b *Batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// Flush writes buffered rows, one batch at a time. On error the unwritten
// rows stay buffered.
func (b *Batcher) Flush(ctx context.Context) error {
	for {
		b.mu.Lock()
		n := min(len(b.buf), b.batchSize())
		batch := append([]JobRow(nil), b.buf[:n]...)
		droppedBefore := b.dropped
		b.mu.Unlock()
		if n == 0 {
			return nil
		}
		if err := b.Sink.WriteRows(ctx, batch); err != nil {
			return err
		}
		b.mu.Lock()
		// Rows dropped from the front while writing were part of this
		// batch; remove only the written rows still buffered.
		k := max(0, min(n-(b.dropped-droppedBefore), len(b.buf)))
		b.buf = b.buf[k:]
		b.mu.Unlock()
	}
}

// Run flushes on every interval or full batch until ctx ends, then makes a
// final best-effort flush.
func (b *Batcher) Run(ctx context.Context) {
	interval := b.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	b.mu.Lock()
	kick := b.kickCh()
	b.mu.Unlock()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			b.flushLogged(final)
			cancel()
			return
		case <-t.C:
		case <-kick:
		}
		b.flushLogged(ctx)
	}
}

func (b *Batcher) flushLogged(ctx context.Context) {
	if err := b.Flush(ctx); err != nil {
		log := b.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("export job records", "err", err, "pending", b.Pending())
	}
}
