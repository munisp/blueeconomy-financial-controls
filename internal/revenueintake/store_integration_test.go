//go:build integration

package revenueintake

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration harness: real PostgreSQL, real migrations (DATABASE_URL +
// MIGRATION_PATH, repo convention). MIGRATION_PATH points at
// 0010_revenue_intake.sql; the harness applies the 0008/0009 chain from the
// same directory first (intake rows reference settlement_records). The
// package gets a DEDICATED database — the harness drops the public schema.
func openStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		t.Fatal("MIGRATION_PATH is required for integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	for _, path := range []string{
		filepath.Join(filepath.Dir(migrationPath), "0008_tariff.sql"),
		filepath.Join(filepath.Dir(migrationPath), "0009_revenue_assurance.sql"),
		migrationPath,
	} {
		statement, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(statement)); err != nil {
			t.Fatalf("apply migration %s: %v", path, err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

// TestRecordAssessmentValidDedupeReplay: a verified event lands once; the
// at-least-once replay is a no-op (idempotency key = event id).
func TestRecordAssessmentValidDedupeReplay(t *testing.T) {
	store, pool := openStore(t)
	ctx := context.Background()
	event, err := fixtureVerifier(t).VerifyAndParse(readFixture(t))
	if err != nil {
		t.Fatalf("verify fixture: %v", err)
	}
	created, err := store.RecordAssessment(ctx, event)
	if err != nil || !created {
		t.Fatalf("first landing created=%v err=%v", created, err)
	}
	created, err = store.RecordAssessment(ctx, event)
	if err != nil || created {
		t.Fatalf("replay created=%v err=%v (must be a no-op)", created, err)
	}
	var count int
	var kid, callRef, currency string
	var total int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(signer_kid), max(call_reference), max(currency), max(total_minor)
		FROM revenue_intake_assessments WHERE event_id = $1`, event.EventID).
		Scan(&count, &kid, &callRef, &currency, &total); err != nil {
		t.Fatal(err)
	}
	if count != 1 || kid != "port-interoperability-1" || callRef != "SBM-2026-0007" ||
		currency != "USD" || total != 2500000 {
		t.Fatalf("landed row: count=%d kid=%s ref=%s %s %d", count, kid, callRef, currency, total)
	}
}

// TestUnverifiedEventNeverLands: the store refuses rows without a verified
// signer kid even if the caller is buggy.
func TestUnverifiedEventNeverLands(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	event, err := fixtureVerifier(t).VerifyAndParse(readFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	event.SignerKeyID = ""
	if _, err := store.RecordAssessment(ctx, event); err == nil {
		t.Fatal("unverified event landed (fail-closed violation)")
	}
}

// TestUnmappableEventLandsWithMappingError: an authentic-but-unmappable
// event is recorded with its mapping error and no amount, so the recon
// pipeline surfaces it rather than guessing.
func TestUnmappableEventLandsWithMappingError(t *testing.T) {
	store, pool := openStore(t)
	ctx := context.Background()
	event, err := fixtureVerifier(t).VerifyAndParse(readFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	event.EventID = "e5f6a7b8-0000-4000-8000-0000000000ee"
	event.Domain, event.CallReference, event.TotalMinor, event.Currency = "", "", nil, ""
	event.MappingError = "assessment domain-payload extension is missing"
	created, err := store.RecordAssessment(ctx, event)
	if err != nil || !created {
		t.Fatalf("landing created=%v err=%v", created, err)
	}
	var mappingError string
	var total *int64
	if err := pool.QueryRow(ctx, `
		SELECT mapping_error, total_minor FROM revenue_intake_assessments WHERE event_id = $1`,
		event.EventID).Scan(&mappingError, &total); err != nil {
		t.Fatal(err)
	}
	if mappingError == "" || total != nil {
		t.Fatalf("unmappable row: mapping_error=%q total=%v", mappingError, total)
	}
}
