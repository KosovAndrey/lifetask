.PHONY: db-up db-down build run stop test vet

PG_BIN  := /usr/lib/postgresql/16/bin
DB_URL  := postgres://lifeplan@/lifeplan?host=/tmp&port=5434
TEST_DB := postgres://lifeplan@/lifeplan_test?host=/tmp&port=5434
SOCK    := $(CURDIR)/tmp/lifeplan.sock

db-up:
	@test -d .pgdata/data || ($(PG_BIN)/initdb -D .pgdata/data -U lifeplan --auth=trust -E UTF8 --locale=C.UTF-8 >/dev/null \
		&& $(PG_BIN)/pg_ctl -D .pgdata/data -o "-p 5434 -k /tmp" -l .pgdata/log start \
		&& sleep 2 && $(PG_BIN)/createdb -h /tmp -p 5434 -U lifeplan lifeplan \
		&& $(PG_BIN)/createdb -h /tmp -p 5434 -U lifeplan lifeplan_test)
	@$(PG_BIN)/pg_ctl -D .pgdata/data status >/dev/null || $(PG_BIN)/pg_ctl -D .pgdata/data -o "-p 5434 -k /tmp" -l .pgdata/log start

db-down:
	$(PG_BIN)/pg_ctl -D .pgdata/data stop

build:
	go build -o bin/lifeplan ./cmd/lifeplan
	go build -o bin/plan ./cmd/plan

run: build stop
	@mkdir -p tmp
	set -a; . ./.env; set +a; DATABASE_URL='$(DB_URL)' LISTEN_ADDR=unix:$(SOCK) nohup bin/lifeplan serve > tmp/server.log 2>&1 &
	@sleep 1; tail -3 tmp/server.log

stop:
	-@pkill -x lifeplan; true

vet:
	test -z "$$(gofmt -l .)" || (gofmt -l . && false)
	go vet ./...

test: vet
	TEST_DATABASE_URL='$(TEST_DB)' go test -p 1 ./...
