package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// gates maps each *sql.DB from OpenDSN to its write gate.
var gates sync.Map // *sql.DB -> *writeGate

type txKey struct{}

// ctxTx is the transaction a context carries, with the database it belongs to.
type ctxTx struct {
	db   *sql.DB
	tx   *sql.Tx
	gate *writeGate
}

// ErrWriteOutsideTx is returned for a write sent to the pool by code whose context carries an open transaction.
var ErrWriteOutsideTx = errors.New("write sent outside the context's open transaction on the same database")

// TxFrom returns the transaction ctx carries for db, or nil.
func TxFrom(ctx context.Context, db *sql.DB) *sql.Tx {
	t, _ := ctx.Value(txKey{}).(*ctxTx)
	if t == nil || t.db != db {
		return nil
	}
	return t.tx
}

// heldByContext reports whether ctx carries an open transaction that holds gate.
func heldByContext(ctx context.Context, gate *writeGate) bool {
	t, _ := ctx.Value(txKey{}).(*ctxTx)
	return t != nil && t.gate == gate
}

// Tx is a transaction scope from Begin: a real transaction, or a savepoint
// inside the transaction the context already carries.
type Tx struct {
	*sql.Tx
	ctx       context.Context
	savepoint bool
	done      bool
}

const savepointName = "nested"

// Begin opens a transaction on db. When ctx already carries one on db, it
// opens a savepoint inside it instead. Commit and Rollback keep their
// meaning for the inner scope and the outer transaction stays the only
// holder of the write lock.
func Begin(ctx context.Context, db *sql.DB) (*Tx, error) {
	if outer := TxFrom(ctx, db); outer != nil {
		if _, err := outer.ExecContext(ctx, "SAVEPOINT "+savepointName); err != nil {
			return nil, err
		}
		return &Tx{Tx: outer, ctx: ctx, savepoint: true}, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, ctx: ctx}, nil
}

// Commit commits the transaction, or releases the savepoint into the outer one.
func (t *Tx) Commit() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	if !t.savepoint {
		return t.Tx.Commit()
	}
	_, err := t.Tx.ExecContext(t.ctx, "RELEASE "+savepointName)
	return err
}

// Rollback undoes the scope's writes; after Commit it does nothing.
func (t *Tx) Rollback() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	if !t.savepoint {
		return t.Tx.Rollback()
	}
	if _, err := t.Tx.ExecContext(t.ctx, "ROLLBACK TO "+savepointName); err != nil {
		return err
	}
	_, err := t.Tx.ExecContext(t.ctx, "RELEASE "+savepointName)
	return err
}

// InTx runs fn with a context carrying one transaction on db, committing when
// fn returns nil. Every store call fn makes with that context writes inside
// the transaction, so the write lock is taken once for all of them.
func InTx(ctx context.Context, db *sql.DB, fn func(ctx context.Context) error) error {
	if TxFrom(ctx, db) != nil {
		return fn(ctx)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	gate, _ := gates.Load(db)
	g, _ := gate.(*writeGate)
	inner := context.WithValue(ctx, txKey{}, &ctxTx{db: db, tx: tx, gate: g})
	if err := fn(inner); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
