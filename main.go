package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

var (
	pathFlag       = flag.String("path", "", "Path to migrations directory")
	databaseFlag   = flag.String("database", "", "Database URL")
	retryDirtyFlag = flag.Bool("retry-dirty", false, "Retry dirty migrations if they are non-transactional and cleaned up")
	verboseFlag    = flag.Bool("verbose", false, "Verbose mode")
	versionFlag    = flag.Bool("version", false, "Show version")
)

var (
	commentRegex     = regexp.MustCompile(`(?s)/\*.*?\*/|--.*?\n`)
	createIndexRegex = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:(?:"?[a-zA-Z0-9_]+"?"?\.)?"?([a-zA-Z0-9_]+)"?)`)
)

func parseConcurrentIndexes(sqlStr string) (bool, []string, error) {
	// Remove comments
	cleanSQL := commentRegex.ReplaceAllString(sqlStr, "\n")

	// Split by semicolon
	statements := strings.Split(cleanSQL, ";")

	var indexNames []string
	for _, stmt := range statements {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}

		// Check if this statement is a CREATE INDEX CONCURRENTLY
		loc := createIndexRegex.FindStringSubmatchIndex(trimmed)
		if loc == nil {
			// Found a statement that is not CREATE INDEX CONCURRENTLY
			return false, nil, nil
		}

		// Extract the index name
		match := createIndexRegex.FindStringSubmatch(trimmed)
		if len(match) > 1 {
			indexNames = append(indexNames, match[1])
		}
	}

	return len(indexNames) > 0, indexNames, nil
}

func recoverDirty(path, dbURL string) error {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	var version int
	var dirty bool
	query := `SELECT version, dirty FROM schema_migrations LIMIT 1`
	err = db.QueryRow(query).Scan(&version, &dirty)
	if err != nil {
		// Table might not exist yet, which is fine
		return nil
	}

	if !dirty {
		return nil
	}

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to read migrations directory: %w", err)
	}

	var migrationFile string
	prefixPadded := fmt.Sprintf("%06d_", version)
	prefixUnpadded := fmt.Sprintf("%d_", version)

	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".up.sql") {
			if strings.HasPrefix(f.Name(), prefixPadded) || strings.HasPrefix(f.Name(), prefixUnpadded) {
				migrationFile = filepath.Join(path, f.Name())
				break
			}
		}
	}

	if migrationFile == "" {
		return fmt.Errorf("migration file for version %d not found", version)
	}

	contentBytes, err := ioutil.ReadFile(migrationFile)
	if err != nil {
		return fmt.Errorf("failed to read migration file: %w", err)
	}
	sqlStr := string(contentBytes)

	ok, indexNames, err := parseConcurrentIndexes(sqlStr)
	if err != nil {
		return fmt.Errorf("failed to parse migration: %w", err)
	}
	if !ok {
		return fmt.Errorf("migration contains statements other than CREATE INDEX CONCURRENTLY")
	}

	for _, indexName := range indexNames {
		var indisvalid bool
		checkQuery := `SELECT indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`
		err := db.QueryRow(checkQuery, indexName).Scan(&indisvalid)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to check index %s: %w", indexName, err)
		}
		return fmt.Errorf("index %s still exists in the database", indexName)
	}

	var updateQuery string
	if version <= 1 {
		updateQuery = `TRUNCATE schema_migrations`
		_, err = db.Exec(updateQuery)
	} else {
		updateQuery = `UPDATE schema_migrations SET version = $1, dirty = false`
		_, err = db.Exec(updateQuery, version-1)
	}

	if err != nil {
		return fmt.Errorf("failed to clear dirty flag: %w", err)
	}

	log.Printf("Successfully recovered from dirty state for version %d. Retrying migration...", version)
	return nil
}

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Println("migrate version 4.15.2-custom")
		return
	}

	if *pathFlag == "" || *databaseFlag == "" {
		flag.Usage()
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) == 0 {
		log.Fatal("Command is required")
	}
	command := args[0]

	if *retryDirtyFlag {
		if err := recoverDirty(*pathFlag, *databaseFlag); err != nil {
			log.Printf("Dirty recovery check failed/skipped: %v", err)
		}
	}

	m, err := migrate.New("file://"+*pathFlag, *databaseFlag)
	if err != nil {
		log.Fatalf("Failed to initialize migrate: %v", err)
	}
	defer m.Close()

	switch command {
	case "up":
		var limit int
		if len(args) > 1 {
			var err error
			limit, err = strconv.Atoi(args[1])
			if err != nil {
				log.Fatalf("Invalid limit: %v", err)
			}
		}
		if limit > 0 {
			err = m.Steps(limit)
		} else {
			err = m.Up()
		}
	case "down":
		var limit int
		if len(args) > 1 {
			var err error
			limit, err = strconv.Atoi(args[1])
			if err != nil {
				log.Fatalf("Invalid limit: %v", err)
			}
		}
		if limit > 0 {
			err = m.Steps(-limit)
		} else {
			err = m.Down()
		}
	case "force":
		if len(args) < 2 {
			log.Fatal("Version is required for force command")
		}
		v, err := strconv.Atoi(args[1])
		if err != nil {
			log.Fatalf("Invalid version: %v", err)
		}
		err = m.Force(v)
	case "version":
		v, dirty, err := m.Version()
		if err != nil {
			log.Fatalf("Failed to get version: %v", err)
		}
		fmt.Printf("Version: %d, Dirty: %t\n", v, dirty)
		return
	case "drop":
		err = m.Drop()
	default:
		log.Fatalf("Unsupported command: %s", command)
	}

	if err != nil {
		if err == migrate.ErrNoChange {
			fmt.Println("No change")
			return
		}
		log.Fatalf("Migration failed: %v", err)
	}

	fmt.Println("Migration successful")
}