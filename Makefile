APP      := chainwatch
COMPOSE  := docker compose -f deploy/docker-compose.yml
FORGE    ?= $(shell command -v forge 2>/dev/null || echo $(HOME)/.foundry/bin/forge)
GO_DIRS  := cmd internal migrations
TEST_DATABASE_URL ?= postgres://chainwatch:chainwatch@localhost:$${CHAINWATCH_PG_PORT:-55432}/chainwatch?sslmode=disable

.PHONY: build test test-integration lint check run docker contracts-test demo clean

build: ## build bin/chainwatch
	CGO_ENABLED=0 go build -trimpath -o bin/$(APP) ./cmd/$(APP)

test: ## unit tests with the race detector
	go test -race ./...

test-integration: ## all tests including Postgres (needs: docker compose up -d postgres)
	CHAINWATCH_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -tags integration ./...

lint: ## go vet (with and without build tags) and gofmt
	go vet ./...
	go vet -tags integration ./...
	@out="$$(gofmt -l $(GO_DIRS))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: lint test ## must pass before any change is done

run: build ## run locally with .env
	set -a && . ./.env && set +a && ./bin/$(APP)

docker: ## build the container image
	docker build -f deploy/Dockerfile -t $(APP):dev .

contracts-test: ## Foundry tests for the demo contracts
	cd contracts && $(FORGE) test

demo: ## end-to-end drain demo on anvil + postgres (see README)
	./scripts/demo.sh

clean:
	rm -rf bin contracts/out contracts/cache contracts/broadcast
