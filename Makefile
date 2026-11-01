# End-to-end flow. Requires Go >= 1.22 and PostgreSQL (or `make db-up` with Docker).
# DATABASE_URL defaults to postgres://recon:recon@localhost:5432/recon?sslmode=disable

export DATABASE_URL ?= postgres://recon:recon@localhost:5432/recon?sslmode=disable
BIN := bin/recon

.PHONY: all build test db-up db-down before fixes after dump lint clean

all: before after

build:
	go build -mod=vendor -o $(BIN) ./cmd/recon

# unit tests + end-to-end test on a fixture (uses a separate, disposable database)
export RECON_TEST_DATABASE_URL ?= postgres://recon:recon@localhost:5432/recon_test?sslmode=disable
test:
	go test -mod=vendor ./...

db-up:
	docker compose up -d --wait

db-down:
	docker compose down -v

# 1) original configs -> ingest -> reconcile -> report
before: build
	$(BIN) load-config
	$(BIN) lint > out/lint_before.txt
	$(BIN) run -out out/report_before_fixes.xlsx -label "BEFORE fixes (configs as provided)" | tee out/run_before.txt

# 2) original configs + MAPPING_FIXES.sql -> re-ingest -> reconcile -> report
after: build
	$(BIN) load-config
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -f MAPPING_FIXES.sql
	$(BIN) lint > out/lint_after.txt
	$(BIN) run -out out/report_after_fixes.xlsx -label "AFTER fixes (MAPPING_FIXES.sql applied)" | tee out/run_after.txt

lint: build
	$(BIN) lint

dump:
	pg_dump "$(DATABASE_URL)" --no-owner --no-privileges | gzip > out/recon_after_fixes.sql.gz

clean:
	rm -rf bin out/*.xlsx out/*.txt
