package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no args"},
		{name: "unknown", args: []string{"sideways"}},
		{name: "extra", args: []string{"up", "now"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			err := run(tt.args, &stdout, &stderr, func(direction, string) (string, error) {
				called = true
				return "", nil
			})
			if !errors.Is(err, errUsage) {
				t.Fatalf("error = %v, want usage", err)
			}
			if called {
				t.Fatal("apply was called")
			}
			if !strings.Contains(stderr.String(), "migrate up") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestRunGuards(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{
			name:    "missing url",
			args:    []string{"up"},
			wantErr: "DATABASE_URL is required",
		},
		{
			name:    "foreign database",
			args:    []string{"up", "-database", "postgres://localhost/other_db?sslmode=disable"},
			wantErr: "not a shortlink database",
		},
		{
			name:    "down on dev database",
			args:    []string{"down", "-database", "postgres://localhost/shortlink"},
			wantErr: "_test",
		},
		{
			name:    "env foreign database",
			args:    []string{"version"},
			env:     "postgres://localhost/other_db",
			wantErr: "not a shortlink database",
		},
		{
			name:    "query dbname foreign database",
			args:    []string{"up", "-database", "postgres://localhost?dbname=other_db"},
			wantErr: "not a shortlink database",
		},
		{
			name:    "keyword dsn",
			args:    []string{"up", "-database", "host=localhost dbname=other_db password=s3cret"},
			wantErr: "invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", tt.env)
			var stdout, stderr bytes.Buffer
			called := false
			err := run(tt.args, &stdout, &stderr, func(direction, string) (string, error) {
				called = true
				return "", nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "password=") {
				t.Fatalf("error leaked database url: %v", err)
			}
			if called {
				t.Fatal("apply was called")
			}
		})
	}
}

func TestAllow(t *testing.T) {
	tests := []struct {
		name    string
		dir     direction
		db      string
		wantErr string
	}{
		{name: "project database", dir: dirUp, db: "shortlink"},
		{name: "test database", dir: dirUp, db: "shortlink_test"},
		{name: "deployed database", dir: dirUp, db: "shortlink_prod"},
		{name: "test database down", dir: dirDown, db: "shortlink_test"},
		{name: "deployed database down", dir: dirDown, db: "shortlink_prod", wantErr: "_test"},
		{name: "foreign database", dir: dirUp, db: "other_db", wantErr: "not a shortlink database"},
		{name: "foreign database version", dir: dirVersion, db: "other_db", wantErr: "not a shortlink database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := allow(tt.dir, tt.db)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("allow: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunUpPrintsVersion(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var stdout, stderr bytes.Buffer
	var gotDir direction
	var gotDSN string
	err := run([]string{"up", "-database", "postgres://localhost/shortlink_test"}, &stdout, &stderr,
		func(dir direction, dsn string) (string, error) {
			gotDir = dir
			gotDSN = dsn
			return "3", nil
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotDir != dirUp || gotDSN != "postgres://localhost/shortlink_test" {
		t.Fatalf("apply dir = %d, dsn = %s", gotDir, gotDSN)
	}
	if stdout.String() != "schema version: 3\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDatabaseName(t *testing.T) {
	tests := []struct {
		dsn     string
		want    string
		wantErr bool
	}{
		{dsn: "postgres://user:pass@localhost:5432/shortlink_test?sslmode=disable", want: "shortlink_test"},
		{dsn: "postgresql://localhost/shortlink_test", want: "shortlink_test"},
		{dsn: "postgres://localhost/other_db", want: "other_db"},
		{dsn: "postgres://localhost?dbname=shortlink_test", want: "shortlink_test"},
		{dsn: "postgres://localhost/shortlink?dbname=other_db", wantErr: true},
		{dsn: "postgres://user:p%ss@localhost/shortlink", wantErr: true},
		{dsn: "host=localhost dbname=other_db password=s3cret", wantErr: true},
		{dsn: "postgres://localhost", wantErr: true},
		{dsn: "://bad", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			got, err := databaseName(tt.dsn)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if strings.Contains(err.Error(), "p%ss") || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), tt.dsn) {
					t.Fatal("error leaked database url")
				}
				return
			}
			if err != nil {
				t.Fatalf("databaseName: %v", err)
			}
			if got != tt.want {
				t.Fatalf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMigrationFilesPaired(t *testing.T) {
	t.Chdir("../..")
	entries, err := readMigrationFiles("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("versions = %d, want 2: %v", len(entries), entries)
	}
	for version := 1; version <= 2; version++ {
		pair, ok := entries[version]
		if !ok || !pair.up || !pair.down {
			t.Fatalf("version %d = %+v, want up and down", version, pair)
		}
	}
}

type migrationPair struct {
	up   bool
	down bool
}

func readMigrationFiles(dir string) (map[int]migrationPair, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pattern := regexp.MustCompile(`^(\d+)_.*\.(up|down)\.sql$`)
	out := map[int]migrationPair{}
	for _, entry := range list {
		if entry.IsDir() {
			return nil, fmt.Errorf("unexpected directory %s", entry.Name())
		}
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("unexpected migration file %s", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, err
		}
		pair := out[version]
		switch match[2] {
		case "up":
			if pair.up {
				return nil, fmt.Errorf("duplicate up for version %d", version)
			}
			pair.up = true
		case "down":
			if pair.down {
				return nil, fmt.Errorf("duplicate down for version %d", version)
			}
			pair.down = true
		}
		out[version] = pair
	}
	return out, nil
}
