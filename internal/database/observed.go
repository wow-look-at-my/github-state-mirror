// Kinds the observed driver reports.

package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"time"
)

// Kinds the observed driver reports. Each is SQLite's own time; the wait for
// the write lock is reported apart, as KindWriteWait.
const (
	KindBegin  = "db.begin"
	KindExec   = "db.exec"
	KindQuery  = "db.query"
	KindCommit = "db.commit"
)

// Observer receives the duration of each SQLite call.
type Observer interface {
	Observe(kind string, d time.Duration)
}

// baseDriver is the registered modernc driver. sql.Open is lazy, so this opens no file.
func baseDriver() driver.Driver {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic("database: sqlite driver is not registered: " + err.Error())
	}
	defer db.Close()
	return db.Driver()
}

// observedConnector opens connections that share one write gate (gate.go). A
// nil obs times nothing.
type observedConnector struct {
	dsn  string
	drv  driver.Driver
	obs  Observer
	gate *writeGate
}

func (c observedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &observedConn{Conn: conn, obs: c.obs, gate: c.gate}, nil
}

func (c observedConnector) Driver() driver.Driver { return c.drv }

// observedConn times the calls database/sql makes.
type observedConn struct {
	driver.Conn
	obs  Observer
	gate *writeGate
	inTx bool
}

func (c *observedConn) timed(kind string, start time.Time) {
	if c.obs != nil {
		c.obs.Observe(kind, time.Since(start))
	}
}

// admit takes the gate for a write on this connection. A context whose own
// transaction already holds it is refused rather than left to wait on itself.
func (c *observedConn) admit(ctx context.Context) error {
	if heldByContext(ctx, c.gate) {
		return ErrWriteOutsideTx
	}
	return c.gate.acquire(ctx)
}

// BeginTx holds the gate until Commit or Rollback.
func (c *observedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	defer c.timed(KindBegin, time.Now())
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		c.gate.release()
		return nil, err
	}
	c.inTx = true
	return &observedTx{Tx: tx, conn: c}, nil
}

// ExecContext outside a transaction is an autocommit write, admitted through the gate for its duration.
func (c *observedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !c.inTx {
		if err := c.admit(ctx); err != nil {
			return nil, err
		}
		defer c.gate.release()
	}
	defer c.timed(KindExec, time.Now())
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

// QueryContext admits an autocommit write (INSERT ... RETURNING) through the
// gate, held until its rows close; reads never wait for it.
func (c *observedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.inTx || !isWriteSQL(query) {
		defer c.timed(KindQuery, time.Now())
		return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	}
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	defer c.timed(KindQuery, time.Now())
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		c.gate.release()
		return nil, err
	}
	return &gatedRows{Rows: rows, gate: c.gate}, nil
}

func (c *observedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *observedConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c *observedConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *observedConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type observedTx struct {
	driver.Tx
	conn *observedConn
}

func (t *observedTx) Commit() error {
	defer t.end()
	defer t.conn.timed(KindCommit, time.Now())
	return t.Tx.Commit()
}

func (t *observedTx) Rollback() error {
	defer t.end()
	return t.Tx.Rollback()
}

func (t *observedTx) end() {
	t.conn.inTx = false
	t.conn.gate.release()
}

// gatedRows releases the gate when a write query's rows close.
type gatedRows struct {
	driver.Rows
	gate *writeGate
	once sync.Once
}

func (r *gatedRows) Close() error {
	defer r.once.Do(r.gate.release)
	return r.Rows.Close()
}
