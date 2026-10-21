package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

type recordedTxDriver struct {
	state *txTestState
}

type txTestState struct {
	begun     int
	committed int
	rolled    int
	queries   []string
}

func (d recordedTxDriver) Open(string) (driver.Conn, error) {
	return &recordedTxConn{state: d.state}, nil
}

type recordedTxConnector struct {
	state *txTestState
}

func (c recordedTxConnector) Connect(context.Context) (driver.Conn, error) {
	return &recordedTxConn{state: c.state}, nil
}

func (c recordedTxConnector) Driver() driver.Driver {
	return recordedTxDriver{state: c.state}
}

type recordedTxConn struct {
	state *txTestState
}

func (c *recordedTxConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not used")
}

func (c *recordedTxConn) Close() error { return nil }

func (c *recordedTxConn) Begin() (driver.Tx, error) {
	c.state.begun++
	return &recordedTx{state: c.state}, nil
}

func (c *recordedTxConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.begun++
	return &recordedTx{state: c.state}, nil
}

func (c *recordedTxConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.queries = append(c.state.queries, strings.TrimSpace(query))
	return &emptyRows{}, nil
}

func (c *recordedTxConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.queries = append(c.state.queries, strings.TrimSpace(query))
	return driver.RowsAffected(1), nil
}

type recordedTx struct {
	state *txTestState
}

func (t *recordedTx) Commit() error {
	t.state.committed++
	return nil
}

func (t *recordedTx) Rollback() error {
	t.state.rolled++
	return nil
}

type emptyRows struct{}

func (*emptyRows) Columns() []string              { return []string{"col"} }
func (*emptyRows) Close() error                   { return nil }
func (*emptyRows) Next([]driver.Value) error      { return io.EOF }

func TestPostgresWithinAccountSampleTxCommitsAndUsesForUpdate(t *testing.T) {
	state := &txTestState{}
	db := sql.OpenDB(recordedTxConnector{state: state})
	defer db.Close()

	repo := NewPostgres(db)
	err := repo.WithinAccountSampleTx(context.Background(), func(txRepo Repository) error {
		if _, err := txRepo.GetCheckpoint(context.Background(), "prod:node-1", "11111111-1111-1111-1111-111111111111"); err != nil {
			return err
		}
		if _, err := txRepo.GetQuotaState(context.Background(), "11111111-1111-1111-1111-111111111111"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithinAccountSampleTx: %v", err)
	}
	if state.begun != 1 || state.committed != 1 || state.rolled != 0 {
		t.Fatalf("expected 1 begin, 1 commit, 0 rollback; got %#v", state)
	}
	if len(state.queries) != 2 {
		t.Fatalf("expected 2 queries, got %d", len(state.queries))
	}
	for i, q := range state.queries {
		if !strings.HasSuffix(q, "FOR UPDATE") {
			t.Fatalf("query %d missing FOR UPDATE: %s", i, q)
		}
	}
}

func TestPostgresWithinAccountSampleTxRollsBackOnError(t *testing.T) {
	state := &txTestState{}
	db := sql.OpenDB(recordedTxConnector{state: state})
	defer db.Close()

	repo := NewPostgres(db)
	boom := errors.New("simulated failure")
	err := repo.WithinAccountSampleTx(context.Background(), func(txRepo Repository) error {
		if _, err := txRepo.GetCheckpoint(context.Background(), "prod:node-1", "11111111-1111-1111-1111-111111111111"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom error, got %v", err)
	}
	if state.begun != 1 || state.committed != 0 || state.rolled != 1 {
		t.Fatalf("expected 1 begin, 0 commit, 1 rollback; got %#v", state)
	}
}
