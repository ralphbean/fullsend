package k8spoc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingInputsFailClosed(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	ctx := context.Background()
	if err := Migrate(ctx); err == nil {
		t.Fatal("migration accepted missing database")
	}
	if err := Status(ctx); err == nil {
		t.Fatal("status accepted missing database")
	}
	if err := Dispatch(ctx); err == nil {
		t.Fatal("dispatch accepted missing database")
	}
	if err := Poll(ctx, "/nonexistent/fixture.json", "/nonexistent/selection.json"); err == nil {
		t.Fatal("poll accepted missing fixture")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	selection := filepath.Join(dir, "selection.json")
	if err := os.WriteFile(fixture, []byte(`{"tenant":"t","source":"s","cursor":"c"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selection, []byte(`{"by_kind":{"opened":"invalid"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Poll(ctx, fixture, selection); err == nil {
		t.Fatal("poll accepted invalid selection")
	}
}

func TestDatabaseReadinessIsBounded(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test@127.0.0.1:1/test?sslmode=disable")
	if _, err := connectUntil(context.Background(), 10*time.Millisecond); err == nil {
		t.Fatal("accepted unavailable database")
	}
}

func TestFixtureValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"tenant":"t","source":"s","cursor":"c","observations":[{"key":"a","entity":"e","revision":"1","kind":"opened","behavior":"success"},{"key":"b","entity":"e","revision":"2","kind":"opened","behavior":"failure"}]}`, true},
		{`{"tenant":"t","source":"s","cursor":"c","observations":[{"key":"a","entity":"e","revision":"1","kind":"opened","behavior":"success"},{"key":"a","entity":"e","revision":"2","kind":"opened","behavior":"failure"}]}`, false},
		{`{"tenant":"t","source":"s","cursor":"c","observations":[{"key":"a","entity":"e","revision":"1","kind":"opened","behavior":"unsafe"}]}`, false},
		{`{"tenant":"t","source":"s"}`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readFixture(path)
		if (err == nil) != tc.valid {
			t.Errorf("body %s: err %v", tc.body, err)
		}
	}
}

func TestSelectionRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selection.json")
	if err := os.WriteFile(path, []byte(`{"by_kind":{"opened":["stub"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, first, err := readSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"by_kind":{"opened":["stub","other"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, second, err := readSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different content retained the same revision")
	}
	if err := os.WriteFile(path, []byte(`{"by_kind":{"opened":[""]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSelection(path); err == nil {
		t.Fatal("accepted empty harness name")
	}
}
