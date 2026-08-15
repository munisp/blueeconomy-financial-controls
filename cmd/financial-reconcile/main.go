package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/reconciliation"
)

const maxStatementBytes = 64 << 20

func main() {
	if err := run(); err != nil {
		log.Printf("financial-reconcile: %v", err)
		os.Exit(1)
	}
}

func run() error {
	databaseURL := requiredEnv("DATABASE_URL")
	statementPath := requiredEnv("STATEMENT_PATH")
	reportPath := requiredEnv("REPORT_PATH")
	if filepath.Clean(statementPath) == filepath.Clean(reportPath) {
		return errors.New("report path must not overwrite statement input")
	}
	statementBytes, err := readRegularFile(statementPath)
	if err != nil {
		return err
	}
	var statements []reconciliation.StatementEntry
	decoder := json.NewDecoder(bytes.NewReader(statementBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&statements); err != nil {
		return fmt.Errorf("decode statement JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("statement file must contain exactly one JSON array")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := intent.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	intents, err := store.ListReconciliationIntents(ctx)
	if err != nil {
		return err
	}
	report, err := reconciliation.Reconcile(intents, statements, statementBytes)
	if err != nil {
		return err
	}
	reportBytes, err := reconciliation.MarshalReport(report)
	if err != nil {
		return err
	}
	if err := writeReport(reportPath, append(reportBytes, '\n')); err != nil {
		return err
	}
	if !report.ReconciliationOK {
		return errors.New("reconciliation produced findings; report written and financial state remains unreleased")
	}
	return nil
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s must be set", name)
	}
	return value
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("statement input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStatementBytes {
		return nil, errors.New("statement input must be a non-empty regular file within the size limit")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read statement input: %w", err)
	}
	return content, nil
}

func writeReport(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".financial-reconcile-*.tmp")
	if err != nil {
		return fmt.Errorf("create report staging file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o640); err != nil {
		temporary.Close()
		return fmt.Errorf("protect report staging file: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write report staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync report staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close report staging file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}
