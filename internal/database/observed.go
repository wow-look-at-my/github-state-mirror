package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"
)

// Kinds the observed driver reports. With _txlock=immediate, db.begin is the
// wait for the write lock.
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

type observedConnector struct {
	dsn string
	drv driver.Driver
	obs Observer
}

func (c observedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &observedConn{Conn: conn, obs: c.obs}, nil
}

func (c observedConnector) Driver() driver.Driver { return c.drv }

// observedConn times the calls database/sql makes.
type observedConn struct {
	driver.Conn
	obs Observer
}

func (c *observedConn) timed(kind string, start time.Time) { c.obs.Observe(kind, time.Since(start)) }

func (c *observedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	defer c.timed(KindBegin, time.Now())
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &observedTx{Tx: tx, obs: c.obs}, nil
}

func (c *observedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	defer c.timed(KindExec, time.Now())
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *observedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	defer c.timed(KindQuery, time.Now())
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
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
	obs Observer
}

func (t *observedTx) Commit() error {
	start := time.Now()
	err := t.Tx.Commit()
	t.obs.Observe(KindCommit, time.Since(start))
	return err
}
